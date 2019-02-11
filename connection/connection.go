/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package connection

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"google.golang.org/grpc"
	"k8s.io/klog"
)

const (
	// Interval of logging connection errors
	connectionLoggingInterval = 10 * time.Second
)

// Connect opens insecure gRPC connection to a CSI driver. Address must be either absolute path to UNIX domain socket
// file or have format '<protocol>://', following gRPC name resolution mechanism at
// https://github.com/grpc/grpc/blob/master/doc/naming.md.
//
// The function tries to connect indefinitely every second until it connects. The function automatically disables TLS
// and adds interceptor for logging of all gRPC messages at level 5.
//
// For a connection to a Unix Domain socket, the behavior after
// loosing the connection is configurable. The default is to
// log the connection loss and reestablish a connection. Applications
// which need to know about a connection loss can be notified by
// passing a callback with OnConnectionLoss and in that callback
// can decide what to do:
// - exit the application with os.Exit
// - invalidate cached information
// - disable the reconnect, which will cause all gRPC method calls to fail with status.Unavailable
//
// For other connections, the default behavior from gRPC is used and
// loss of connection is not detected reliably.
func Connect(address string, options ...Option) (*grpc.ClientConn, error) {
	return connect(address, []grpc.DialOption{}, options)
}

// Option is the type of all optional parameters for Connect.
type Option func(o *options)

// OnConnectionLoss registers a callback that will be invoked when the
// connection got lost. If that callback returns true, the connection
// is restablished. Otherwise the connection is left as it is and
// all future gRPC calls using it will fail with status.Unavailable.
func OnConnectionLoss(reconnect func() bool) Option {
	return func(o *options) {
		o.reconnect = reconnect
	}
}

type options struct {
	reconnect func() bool
}

// connect is the internal implementation of Connect. It has more options to enable testing.
func connect(address string, dialOptions []grpc.DialOption, connectOptions []Option) (*grpc.ClientConn, error) {
	var o options
	for _, option := range connectOptions {
		option(&o)
	}

	dialOptions = append(dialOptions,
		grpc.WithInsecure(),                   // Don't use TLS, it's usually local Unix domain socket in a container.
		grpc.WithBackoffMaxDelay(time.Second), // Retry every second after failure.
		grpc.WithBlock(),                      // Block until connection succeeds.
		grpc.WithUnaryInterceptor(LogGRPC),    // Log all messages.
	)
	unixPrefix := "unix://"
	if strings.HasPrefix(address, "/") {
		// It looks like filesystem path.
		address = unixPrefix + address
	}

	if strings.HasPrefix(address, unixPrefix) {
		// state variables for the custom dialer
		haveConnected := false
		lostConnection := false
		reconnect := true

		dialOptions = append(dialOptions, grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			if haveConnected && !lostConnection {
				// We have detected a loss of connection for the first time. Decide what to do...
				// Record this once. TODO (?): log at regular time intervals.
				klog.Errorf("Lost connection to %s.", address)
				// Inform caller and let it decide? Default is to reconnect.
				if o.reconnect != nil {
					reconnect = o.reconnect()
				}
				lostConnection = true
			}
			if !reconnect {
				return nil, errors.New("connection lost, reconnecting disabled")
			}
			conn, err := net.DialTimeout("unix", address[len(unixPrefix):], timeout)
			if err == nil {
				// Connection restablished.
				haveConnected = true
				lostConnection = false
			}
			return conn, err
		}))
	} else if o.reconnect != nil {
		return nil, errors.New("OnConnectionLoss callback only supported for unix:// addresses")
	}

	klog.Infof("Connecting to %s", address)

	// Connect in background.
	var conn *grpc.ClientConn
	var err error
	ready := make(chan bool)
	go func() {
		conn, err = grpc.Dial(address, dialOptions...)
		close(ready)
	}()

	// Log error every connectionLoggingInterval
	ticker := time.NewTicker(connectionLoggingInterval)
	defer ticker.Stop()

	// Wait until Dial() succeeds.
	for {
		select {
		case <-ticker.C:
			klog.Warningf("Still connecting to %s", address)

		case <-ready:
			return conn, err
		}
	}
}

// LogGRPC is gPRC unary interceptor for logging of CSI messages at level 5. It removes any secrets from the message.
func LogGRPC(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	klog.V(5).Infof("GRPC call: %s", method)
	klog.V(5).Infof("GRPC request: %s", protosanitizer.StripSecrets(req))
	err := invoker(ctx, method, req, reply, cc, opts...)
	klog.V(5).Infof("GRPC response: %s", protosanitizer.StripSecrets(reply))
	klog.V(5).Infof("GRPC error: %v", err)
	return err
}

func GetDriverName(ctx context.Context, conn *grpc.ClientConn) (string, error) {
	client := csi.NewIdentityClient(conn)

	req := csi.GetPluginInfoRequest{}
	rsp, err := client.GetPluginInfo(ctx, &req)
	if err != nil {
		return "", err
	}
	name := rsp.GetName()
	if name == "" {
		return "", fmt.Errorf("driver name is empty")
	}
	return name, nil
}