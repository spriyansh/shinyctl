package lb

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"

	"github.com/caddyserver/caddy/v2"
)

// AdminPath is the control-socket route the table is read and replaced
// through.
const AdminPath = "/shinyctl/copies"

// adminCopies mounts the route. The engine creates a new instance every time
// its control server is rebuilt, so it holds nothing: the table is the
// package's.
type adminCopies struct{}

func (adminCopies) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "admin.api.shinyctl_copies",
		New: func() caddy.Module { return new(adminCopies) },
	}
}

func (adminCopies) Routes() []caddy.AdminRoute {
	return []caddy.AdminRoute{{Pattern: AdminPath, Handler: caddy.AdminHandlerFunc(handleCopies)}}
}

// PIDHeader names, on the table's route, the process that answers, once it
// serves its settings (see MarkServing).
const PIDHeader = "X-Shinyctl-Pid"

var servingPID atomic.Int64

// MarkServing records that this process, pid, has loaded its settings and
// serves every app's port.
func MarkServing(pid int) { servingPID.Store(int64(pid)) }

// maxTableBody caps a pushed table; the control socket is private, so this
// only stops a mistake.
const maxTableBody = 4 << 20

func handleCopies(w http.ResponseWriter, r *http.Request) error {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		if p := servingPID.Load(); p > 0 {
			w.Header().Set(PIDHeader, strconv.FormatInt(p, 10))
		}
		return json.NewEncoder(w).Encode(Current())

	case http.MethodPut:
		var t Table
		if err := json.NewDecoder(io.LimitReader(r.Body, maxTableBody)).Decode(&t); err != nil {
			return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: err}
		}
		if err := Validate(t); err != nil {
			return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: err}
		}
		Set(t)
		return nil
	}
	return caddy.APIError{HTTPStatus: http.StatusMethodNotAllowed, Err: fmt.Errorf("method not allowed")}
}

var _ caddy.AdminRouter = adminCopies{}
