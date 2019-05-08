package app

import (
	"context"
	"log"
	"net"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// TunnelLimits encapsulates bandwidth limits for a given tunnel.
type TunnelLimits struct {
	// Overall tunnel bandwidth limit. Total bandwidth usage by a tunnel never
	// exceeds this value
	TunnelLimit Limit
	// Bandwidth limit for individual connections of this tunnel. No single
	// connection made as a part of this tunnel is allowed to exceed this limit.
	ConnectionLimit Limit
}

// Tunnel is a structure that contains everything you might need to manage an
// existing TCP tunnel
type Tunnel struct {
	limitsUpdate chan<- TunnelLimits
	shutdown     chan<- struct{}
	waitGroup    *sync.WaitGroup
}

// UpdateLimits sets new bandwidth limits for a tunnel. All active connections
// of given tunnel are notified and have their limits updated as well.
func (t Tunnel) UpdateLimits(newLimits TunnelLimits) {
	t.limitsUpdate <- newLimits
}

// Shutdown shuts the tunnel down and blocks until shutdown process is complete.
// This means waiting until all connections and listening socket get close.
func (t Tunnel) Shutdown() {
	close(t.shutdown)
	t.waitGroup.Wait()
}

// CreateTunnel creates a traffic forwarding tunnel with a given listen port
// spec and configuration and returns a structure containing control channels
// for the new tunnel.
func CreateTunnel(listenAt ListenAt, connectTo ConnectTo, limits TunnelLimits) (Tunnel, error) {
	shutdown := make(chan struct{})
	limitsUpdate := make(chan TunnelLimits)
	wg := new(sync.WaitGroup)

	log.Printf("Starting tunnel at %q", listenAt)

	l, err := net.Listen("tcp", string(listenAt))
	if err != nil {
		log.Printf("Failed to listen at %q: %v", listenAt, err)
		return Tunnel{}, err
	}
	// It's internalTunnel's run() responsibility to close the listener
	ti := &tunnelInternals{
		connectTo:       connectTo,
		limitsUpdate:    limitsUpdate,
		shutdown:        shutdown,
		listener:        l,
		tunnelLimiter:   rate.NewLimiter(rate.Limit(limits.TunnelLimit), ForwarderBufSize),
		connectionLimit: limits.ConnectionLimit,
		waitGroup:       wg,
	}

	wg.Add(1)
	go func() {
		defer wg.Done()

		retry := make(chan struct{})

		for {
			if ti.listener != nil {
				err := ti.run(string(listenAt))
				if err == nil {
					return
				}
				// err is not nil, which means that there was an error trying to accept
				// connection. This means that listening socket is no longer in a valid
				// state. Retry listening
				err = ti.listener.Close()
				if err != nil {
					log.Printf("Failed to close listening socket for %q after discovering "+
						"accept failure: %v", listenAt, err)
				}
				ti.listener = nil
				log.Printf("Failed to accept connection on listener %q: %v", listenAt, err)
			}

			go func() {
				time.Sleep(5 * time.Second)
				retry <- struct{}{}
			}()

			select {
			case <-retry:
				l, err := net.Listen("tcp", string(listenAt))
				if err != nil {
					log.Printf("Failed to listen at %q: %v", listenAt, err)
				} else {
					ti.listener = l
				}
			case <-shutdown:
				log.Printf("Detected tunnel shutdown while retrying listening at %q", listenAt)
				return
			} // select
		} // for
	}()

	return ti.toTunnel(), nil
}

type tunnelInternals struct {
	connectTo       ConnectTo
	limitsUpdate    chan TunnelLimits
	shutdown        chan struct{}
	listener        net.Listener
	tunnelLimiter   *rate.Limiter
	connectionLimit Limit
	waitGroup       *sync.WaitGroup
}

func (ti tunnelInternals) toTunnel() Tunnel {
	return Tunnel{
		limitsUpdate: ti.limitsUpdate,
		shutdown:     ti.shutdown,
		waitGroup:    ti.waitGroup,
	}
}

type acceptedConnection struct {
	connection net.Conn
	err        error
}

func (ti *tunnelInternals) run(id string) error {
	pendingConnection := make(chan acceptedConnection)
	// In the very worst case we might find ourselves with an accepted connection
	// in the pendingConnection channel that haven't been read out of it. That's
	// why we are first deferring pendingConnection cleanup and only then
	// deferring listener close (we want to have listener closed and acceptor
	// thread failed with an error before we initiate pendingConnection wipe)
	defer func() {
		select {
		case c := <-pendingConnection:
			if c.connection != nil {
				c.connection.Close()
			}
		default:
		}
	}()
	defer ti.listener.Close()

	// Start acceptor goroutine. It accepts incoming connections and sends them
	// to pendingConnection channel.
	go func() {
		for {
			conn, err := ti.listener.Accept()
			if err != nil {
				pendingConnection <- acceptedConnection{
					connection: nil,
					err:        err,
				}
				return
			}
			pendingConnection <- acceptedConnection{
				connection: conn,
				err:        nil,
			}
		}
	}()

	activeConnections := make(map[*connection]struct{})
	completeChan := make(chan connectionComplete)
	defer func() {
		for conn := range activeConnections {
			conn.close()
		}
	}()

	for {
		select {
		case netConn := <-pendingConnection:
			if netConn.err != nil {
				// We were unable to accept connection. I believe it's safe to assume
				// that listening socket is no longer alive and therefore all
				// connections previously accepted on that socket are dead as well.
				// Which means it's probably safe to return (shutdown all active
				// connections and try to reestablish the listener)
				log.Printf("Detected that we are unable to accept connection at %q: %v", id, netConn.err)
				return netConn.err
			}

			log.Printf("Accepted connection at %q", id)

			conn, err := newConnection{
				ingress:         netConn.connection,
				connectTo:       ti.connectTo,
				waitGroup:       ti.waitGroup,
				complete:        completeChan,
				tunnelLimiter:   ti.tunnelLimiter,
				connectionLimit: ti.connectionLimit,
			}.create()
			if err != nil {
				log.Printf("Failed to connect to %q: %v", ti.connectTo, err)
				netConn.connection.Close()
			} else {
				activeConnections[conn] = struct{}{}
			}
		case complete := <-completeChan:
			if complete.err != nil {
				log.Printf("Connection completed with failure: %v", complete.err)
			}
			_, ok := activeConnections[complete.connection]
			if ok {
				delete(activeConnections, complete.connection)
				complete.connection.close()
				log.Printf("Closed connection at %q", id)
			}
		case limits := <-ti.limitsUpdate:
			ti.tunnelLimiter.SetLimit(rate.Limit(limits.TunnelLimit))
			ti.connectionLimit = limits.ConnectionLimit
			for conn := range activeConnections {
				conn.connectionLimiter.SetLimit(rate.Limit(limits.ConnectionLimit))
			}
			log.Printf("Tunnel at %q limits updated: %v", id, limits)
		case <-ti.shutdown:
			log.Printf("Tunnel at %q shutting down", id)
			return nil
		} // select
	} // for
}

type connection struct {
	connectionLimiter *rate.Limiter
	close             func()
}

type connectionComplete struct {
	connection *connection
	err        error
}

type newConnection struct {
	ingress         net.Conn
	connectTo       ConnectTo
	waitGroup       *sync.WaitGroup
	complete        chan<- connectionComplete
	tunnelLimiter   *rate.Limiter
	connectionLimit Limit
}

func (c newConnection) create() (*connection, error) {
	egress, err := net.Dial("tcp", string(c.connectTo))
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := &connection{
		close: func() {
			cancel()
			err := egress.Close()
			if err != nil {
				log.Printf("Failed to close egress connection: %v", err)
			}
			err = c.ingress.Close()
			if err != nil {
				log.Printf("Failed to close ingress connection: %v", err)
			}
		},
		connectionLimiter: rate.NewLimiter(rate.Limit(c.connectionLimit), ForwarderBufSize),
	}

	c.waitGroup.Add(1)
	limiters := []*rate.Limiter{c.tunnelLimiter, result.connectionLimiter}
	go func() {
		defer c.waitGroup.Done()
		forwardWithCompletion{
			from:     c.ingress,
			to:       egress,
			limiters: limiters,
			complete: c.complete,
		}.run(ctx, result)
	}()

	c.waitGroup.Add(1)
	go func() {
		defer c.waitGroup.Done()
		forwardWithCompletion{
			from:     egress,
			to:       c.ingress,
			limiters: limiters,
			complete: c.complete,
		}.run(ctx, result)
	}()

	return result, nil
}

type forwardWithCompletion struct {
	from     net.Conn
	to       net.Conn
	limiters []*rate.Limiter
	complete chan<- connectionComplete
}

func (fwc forwardWithCompletion) run(ctx context.Context, conn *connection) {
	err := forward{
		from:     fwc.from,
		to:       fwc.to,
		limiters: fwc.limiters,
	}.run(ctx)
	if err != nil {
		log.Printf("Failed to forward egress to ingress: %v", err)
	}

	select {
	case fwc.complete <- connectionComplete{
		connection: conn,
		err:        err,
	}:
	case <-ctx.Done():
		// If we've been canceled then no one expects our completion report
	} // select
}
