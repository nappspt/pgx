package pool

import (
	"errors"
	"github.com/jackc/pgx"
	"sync"
)

type ConnPoolConfig struct {
	pgx.ConnConfig
	MaxConnections int                   // max simultaneous connections to use, default 5, must be at least 2
	AfterConnect   func(*pgx.Conn) error // function to call on every new connection
}

type ConnPool struct {
	allConnections       []*pgx.Conn
	availableConnections []*pgx.Conn
	cond                 *sync.Cond
	config               pgx.ConnConfig // config used when establishing connection
	maxConnections       int
	afterConnect         func(*pgx.Conn) error
	logger               pgx.Logger
	logLevel             int
	closed               bool
}

type ConnPoolStat struct {
	MaxConnections       int // max simultaneous connections to use
	CurrentConnections   int // current live connections
	AvailableConnections int // unused live connections
}

// NewConnPool creates a new ConnPool. config.ConnConfig is passed through to
// Connect directly.
func NewConnPool(config ConnPoolConfig) (p *ConnPool, err error) {
	p = new(ConnPool)
	p.config = config.ConnConfig
	p.maxConnections = config.MaxConnections
	if p.maxConnections == 0 {
		p.maxConnections = 5
	}
	if p.maxConnections < 1 {
		return nil, errors.New("MaxConnections must be at least 1")
	}

	p.afterConnect = config.AfterConnect

	if config.LogLevel != 0 {
		p.logLevel = config.LogLevel
	} else {
		// Preserve pre-LogLevel behavior by defaulting to LogLevelDebug
		p.logLevel = pgx.LogLevelDebug
	}
	p.logger = config.Logger
	if p.logger == nil {
		p.logLevel = pgx.LogLevelNone
	}

	p.allConnections = make([]*pgx.Conn, 0, p.maxConnections)
	p.availableConnections = make([]*pgx.Conn, 0, p.maxConnections)
	p.cond = sync.NewCond(new(sync.Mutex))

	// Initially establish one connection
	var c *pgx.Conn
	c, err = p.createConnection()
	if err != nil {
		return
	}
	p.allConnections = append(p.allConnections, c)
	p.availableConnections = append(p.availableConnections, c)

	return
}

// Acquire takes exclusive use of a connection until it is released.
func (p *ConnPool) Acquire() (c *pgx.Conn, err error) {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	if p.closed {
		return nil, errors.New("cannot acquire from closed pool")
	}

	// A connection is available
	if len(p.availableConnections) > 0 {
		c = p.availableConnections[len(p.availableConnections)-1]
		p.availableConnections = p.availableConnections[:len(p.availableConnections)-1]
		return
	}

	// No connections are available, but we can create more
	if len(p.allConnections) < p.maxConnections {
		c, err = p.createConnection()
		if err != nil {
			return
		}
		p.allConnections = append(p.allConnections, c)
		return
	}

	// All connections are in use and we cannot create more
	if len(p.availableConnections) == 0 {
		if p.logLevel >= pgx.LogLevelWarn {
			p.logger.Warn("All connections in pool are busy - waiting...")
		}
		for len(p.availableConnections) == 0 {
			p.cond.Wait()
		}
	}

	c = p.availableConnections[len(p.availableConnections)-1]
	p.availableConnections = p.availableConnections[:len(p.availableConnections)-1]

	return
}

// Release gives up use of a connection.
func (p *ConnPool) Release(conn *pgx.Conn) {
	if conn.TxStatus != 'I' {
		conn.Exec("rollback")
	}

	if len(conn.channels) > 0 {
		if err := conn.Unlisten("*"); err != nil {
			conn.die(err)
		}
		conn.channels = make(map[string]struct{})
	}
	conn.notifications = nil

	p.cond.L.Lock()

	inConnPool := false
	for _, c := range p.allConnections {
		if conn == c {
			inConnPool = true
			break
		}
	}

	if !inConnPool {
		conn.Close()
		p.cond.L.Unlock()
		p.cond.Signal()
		return
	}

	if conn.IsAlive() {
		p.availableConnections = append(p.availableConnections, conn)
	} else {
		ac := p.allConnections
		for i, c := range ac {
			if conn == c {
				ac[i] = ac[len(ac)-1]
				p.allConnections = ac[0 : len(ac)-1]
				break
			}
		}
	}
	p.cond.L.Unlock()
	p.cond.Signal()
}

// Close ends the use of a connection pool. It prevents any new connections
// from being acquired, waits until all acquired connections are released,
// then closes all underlying connections.
func (p *ConnPool) Close() {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	p.closed = true

	// Wait until all connections are released
	if len(p.availableConnections) != len(p.allConnections) {
		for len(p.availableConnections) != len(p.allConnections) {
			p.cond.Wait()
		}
	}

	for _, c := range p.allConnections {
		_ = c.Close()
	}
}

// Reset closes all open connections, but leaves the pool open. It is intended
// for use when an error is detected that would disrupt all connections (such as
// a network interruption or a server state change).
//
// It is safe to reset a pool while connections are checked out. Those
// connections will be closed when they are returned to the pool.
func (p *ConnPool) Reset() {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	p.allConnections = make([]*pgx.Conn, 0, p.maxConnections)
	p.availableConnections = make([]*pgx.Conn, 0, p.maxConnections)
}

// Stat returns connection pool statistics
func (p *ConnPool) Stat() (s ConnPoolStat) {
	p.cond.L.Lock()
	defer p.cond.L.Unlock()

	s.MaxConnections = p.maxConnections
	s.CurrentConnections = len(p.allConnections)
	s.AvailableConnections = len(p.availableConnections)
	return
}

func (p *ConnPool) createConnection() (c *pgx.Conn, err error) {
	c, err = pgx.Connect(p.config)
	if err != nil {
		return
	}
	if p.afterConnect != nil {
		err = p.afterConnect(c)
		if err != nil {
			return
		}
	}
	return
}

// Exec acquires a connection, delegates the call to that connection, and releases the connection
func (p *ConnPool) Exec(sql string, arguments ...interface{}) (commandTag pgx.CommandTag, err error) {
	var c *pgx.Conn
	if c, err = p.Acquire(); err != nil {
		return
	}
	defer p.Release(c)

	return c.Exec(sql, arguments...)
}

// Query acquires a connection and delegates the call to that connection. When
// *Rows are closed, the connection is released automatically.
func (p *ConnPool) Query(sql string, args ...interface{}) (pgx.Rows, error) {
	c, err := p.Acquire()
	if err != nil {
		// Because checking for errors can be deferred to the *rows, build one with the error
		return pgx.rows{closed: true, err: err}, err
	}

	rows, err := c.Query(sql, args...)
	if err != nil {
		p.Release(c)
		return rows, err
	}

	rows.pool = p
	return rows, nil
}

// QueryRow acquires a connection and delegates the call to that connection. The
// connection is released automatically after Scan is called on the returned
// *Row.
func (p *ConnPool) QueryRow(sql string, args ...interface{}) *pgx.Row {
	rows, _ := p.Query(sql, args...)
	return (*pgx.Row)(rows)
}

// Begin acquires a connection and begins a transaction on it. When the
// transaction is closed the connection will be automatically released.
func (p *ConnPool) Begin() (*pgx.Tx, error) {
	return p.BeginIso("")
}

// BeginIso acquires a connection and begins a transaction in isolation mode iso
// on it. When the transaction is closed the connection will be automatically
// released.
func (p *ConnPool) BeginIso(iso string) (*pgx.Tx, error) {
	for {
		c, err := p.Acquire()
		if err != nil {
			return nil, err
		}

		tx, err := c.BeginIso(iso)
		if err != nil {
			alive := c.IsAlive()
			p.Release(c)

			// If connection is still alive then the error is not something trying
			// again on a new connection would fix, so just return the error. But
			// if the connection is dead try to acquire a new connection and try
			// again.
			if alive {
				return nil, err
			} else {
				continue
			}
		}

		tx.pool = p
		return tx, nil
	}
}