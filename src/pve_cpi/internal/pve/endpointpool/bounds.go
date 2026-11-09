package endpointpool

import (
	"net/http/httptrace"
	"sync"
	"time"
)

// The limits of one walk. A walk makes at most maxHops hops. Before each hop
// after the first it checks two budgets, and it starts no new hop once it has
// spent connectBudget in connect attempts or once walkBudget has passed since
// it began. A hop that has already started finishes.
const (
	maxHops       = 4
	connectBudget = 30 * time.Second
	walkBudget    = 90 * time.Second
)

// bounds are the limits one walk runs under, with the clock that measures its
// budgets. RoundTrip uses defaultBounds, and the package's tests pass their own
// clock to roundTrip.
type bounds struct {
	now     func() time.Time
	hops    int
	connect time.Duration
	total   time.Duration
}

// defaultBounds returns the limits a walk runs under in production.
func defaultBounds() bounds {
	return bounds{now: time.Now, hops: maxHops, connect: connectBudget, total: walkBudget}
}

// hopLimit returns how many hops a walk over the given number of eligible
// endpoints may make, which is one per endpoint up to the hop cap.
func (b bounds) hopLimit(eligible int) int {
	return min(eligible, b.hops)
}

// finalHop reports whether hop i, counted from zero, is the final hop of a walk
// that may make n hops. That is the fourth hop or the hop to the last eligible
// endpoint. The request it carries never has a response-header deadline, so a
// slow but healthy endpoint still answers a slow read on the walk's last try.
// A hop that the budgets turn into the last one is not final by this rule and
// keeps its deadline.
func finalHop(i, n int) bool {
	return i >= n-1
}

// begin starts the budgets of one walk.
func (b bounds) begin() *budget {
	return &budget{bounds: b, start: b.now()}
}

// budget is the time one walk has spent. It is used by one goroutine.
type budget struct {
	bounds  bounds
	start   time.Time
	connect time.Duration
}

// spend adds connect time.
func (w *budget) spend(d time.Duration) {
	if d > 0 {
		w.connect += d
	}
}

// allowsHop reports whether a new hop may start. The walk checks it before
// each hop after the first.
func (w *budget) allowsHop() bool {
	return w.connect < w.bounds.connect && w.bounds.now().Sub(w.start) < w.bounds.total
}

// connectMeter measures the time one hop spends getting a connection, and
// whether it ever got one, through the transport's GetConn and GotConn trace
// hooks. Go calls GetConn before it looks for a connection and GotConn once it
// has one, idle or new, so the time between them is the hop's connect time,
// and a hop whose GotConn never ran wrote no byte. Go's own retry of a request
// calls the pair again, and every round counts.
type connectMeter struct {
	now func() time.Time

	mu      sync.Mutex
	since   time.Time
	waiting bool
	spent   time.Duration
	gotConn bool
}

func newConnectMeter(now func() time.Time) *connectMeter {
	return &connectMeter{now: now}
}

// trace returns the hooks to put on the hop's context. httptrace composes them
// with any trace the caller already put there.
func (m *connectMeter) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.stopLocked()
			m.since = m.now()
			m.waiting = true
		},
		GotConn: func(httptrace.GotConnInfo) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.stopLocked()
			m.gotConn = true
		},
	}
}

func (m *connectMeter) stopLocked() {
	if !m.waiting {
		return
	}
	if d := m.now().Sub(m.since); d > 0 {
		m.spent += d
	}
	m.waiting = false
}

// result ends the measurement, counting a wait that ended in a failure, and
// returns the connect time and whether the hop ever got a connection.
func (m *connectMeter) result() (time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked()
	return m.spent, m.gotConn
}

// failures collects the hops of a walk that got no answer.
type failures struct {
	attempts []EndpointFailure
	// other is the last hop that failed with something other than a
	// connect-phase failure or a header-deadline expiry, such as a reset of a
	// read, or nil when every hop was unreachable.
	other error
}

// add records one failed hop. unreachable says whether it failed in the
// connect phase or on the header deadline.
func (f *failures) add(endpoint string, err error, unreachable bool) {
	f.attempts = append(f.attempts, EndpointFailure{Endpoint: endpoint, Err: err})
	if !unreachable && err != nil {
		f.other = err
	}
}

// err returns the error a walk that ended without an answer gives back. When
// every hop failed in the connect phase or on the header deadline it is an
// *EntryUnreachableError listing each endpoint with its cause, which unwraps
// to one dial-shaped *net.OpError, or to the certificate errors when every hop
// failed verification or its pin. Otherwise it is the last hop's error that
// was not unreachable, unchanged. That error is what makes the walk
// retryable, so a connect-phase failure on a later hop, such as a certificate
// error the SDK never retries, never takes its place.
func (f *failures) err() error {
	if f.other != nil {
		return f.other
	}
	return NewEntryUnreachableError(f.attempts)
}
