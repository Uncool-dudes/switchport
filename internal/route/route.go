package route

// Process identifies one process across time. The start time tells it apart
// from a later process that reuses the same pid.
type Process struct {
	PID     int    `json:"pid"`
	Started string `json:"started"`
}

// App is a route to a command started by switchport run.
type App struct {
	Name     Name      `json:"name"`
	Addr     string    `json:"addr"` // host:port the command listens on
	H2C      bool      `json:"h2c,omitempty"`
	Worktree string    `json:"worktree,omitempty"` // branch prefix, if any
	PID      int       `json:"pid"`                // the switchport run wrapper
	Procs    []Process `json:"procs"`              // the command's process tree
}

// Alias is a static route to a port switchport did not start.
type Alias struct {
	Name Name   `json:"name"`
	Addr string `json:"addr"`
	H2C  bool   `json:"h2c,omitempty"`
}
