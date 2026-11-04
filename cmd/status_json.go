/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"encoding/json"
	"io"
	"slices"
	"time"

	"github.com/spriyansh/shinyctl/internal/api"
	"github.com/spriyansh/shinyctl/internal/manifest"
	"github.com/spriyansh/shinyctl/internal/registry"
)

// statusAsJSON is the --json flag of status.
var statusAsJSON bool

// statusDoc is the document of status --json. Every key is always there, a
// list is [] and never null, and null means unknown, not inspected, or does
// not apply. The keys are the manifest's, with _ for -.
type statusDoc struct {
	Schema       int          `json:"schema"`
	Time         string       `json:"time"`
	API          apiDoc       `json:"api"`
	Apps         []appDoc     `json:"apps"`
	LoadBalancer *balancerDoc `json:"load_balancer"`
	Machine      *machineDoc  `json:"machine"`
	Notes        []string     `json:"notes"`
}

type apiDoc struct {
	Running bool `json:"running"`
}

type appDoc struct {
	Name           string    `json:"name"`
	Group          *string   `json:"group"`
	Host           string    `json:"host"`
	Port           int       `json:"port"`
	Stopped        bool      `json:"stopped"`
	Serving        *bool     `json:"serving"`
	Replicas       int       `json:"replicas"`
	WarmReplicas   int       `json:"warm_replicas"`
	RestartPolicy  *string   `json:"restart_policy"`
	MaxMemoryBytes *int64    `json:"max_memory_bytes"`
	NotServed      *string   `json:"not_served"`
	NotStarted     *string   `json:"not_started"`
	Copies         []copyDoc `json:"copies"`
	Notes          []string  `json:"notes"`
}

type copyDoc struct {
	Copy             int     `json:"copy"`
	Role             *string `json:"role"`
	Status           string  `json:"status"`
	Readiness        *string `json:"readiness"`
	PID              *int    `json:"pid"`
	Alive            bool    `json:"alive"`
	StartedAt        *string `json:"started_at"`
	Crashes          int     `json:"crashes"`
	Sessions         *int    `json:"sessions"`
	MemoryBytes      *int64  `json:"memory_bytes"`
	Outdated         bool    `json:"outdated"`
	SkipDrain        bool    `json:"skip_drain"`
	DrainEndsAt      *string `json:"drain_ends_at"`
	LastExitReason   *string `json:"last_exit_reason"`
	PageProblem      *string `json:"page_problem"`
	PageWaitingSince *string `json:"page_waiting_since"`
}

type balancerDoc struct {
	Starting    bool   `json:"starting"`
	MemoryBytes *int64 `json:"memory_bytes"`
}

type machineDoc struct {
	Low            bool    `json:"low"`
	TotalBytes     int64   `json:"total_bytes"`
	AvailableBytes int64   `json:"available_bytes"`
	SwapTotalBytes int64   `json:"swap_total_bytes"`
	SwapUsedBytes  int64   `json:"swap_used_bytes"`
	StallPercent   *int    `json:"stall_percent"`
	Pressure       *string `json:"pressure"`
}

func strOrNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// timeOrNull is a time as RFC 3339 in UTC, so that jq's fromdate reads it as
// it is. A zero time is null.
func timeOrNull(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func bytesOrNull(n int64) *int64 {
	if n <= 0 {
		return nil
	}
	return &n
}

// serving says a new visitor reaches a ready copy: some copy is alive and
// ready and either the load balancer's active one or a copy from before the
// load balancer (copy 0), which is ready when the app's own port answers.
func serving(app api.AppStatus) bool {
	return slices.ContainsFunc(app.Copies, func(c api.CopyStatus) bool {
		status := statusWord(app, c)
		return status == "alive" && readyWord(status, c) == "ready" && (c.Role == "active" || c.Copy == 0)
	})
}

// statusJSON builds the document of a running shinyctl-api. It is pure: now
// is the clock the notes used.
func statusJSON(resp api.StatusResponse, now time.Time) statusDoc {
	doc := statusDoc{
		Schema:       1,
		Time:         now.UTC().Format(time.RFC3339),
		API:          apiDoc{Running: true},
		Apps:         []appDoc{},
		LoadBalancer: &balancerDoc{Starting: resp.BalancerStarting, MemoryBytes: bytesOrNull(resp.BalancerMemory)},
		Notes:        append([]string{}, topNotes(resp)...),
	}
	if m := resp.MachineMemory; m != nil {
		md := &machineDoc{
			Low:            m.Low,
			TotalBytes:     m.TotalBytes,
			AvailableBytes: m.AvailableBytes,
			SwapTotalBytes: m.SwapTotalBytes,
			SwapUsedBytes:  m.SwapUsedBytes,
			Pressure:       strOrNull(m.Pressure),
		}
		if m.Pressure == "" {
			stall := m.StallPercent
			md.StallPercent = &stall
		}
		doc.Machine = md
	}

	for _, app := range resp.Apps {
		serves := serving(app)
		ad := appDoc{
			Name:           app.Name,
			Group:          strOrNull(app.Group),
			Host:           app.Host,
			Port:           app.Port,
			Stopped:        app.Stopped,
			Serving:        &serves,
			Replicas:       app.Replicas,
			WarmReplicas:   app.WarmReplicas,
			RestartPolicy:  strOrNull(app.RestartPolicy),
			MaxMemoryBytes: bytesOrNull(app.MaxMemoryBytes),
			NotServed:      strOrNull(app.NotServed),
			NotStarted:     strOrNull(app.NotStarted),
			Copies:         []copyDoc{},
			Notes:          append([]string{}, appNotes(app, now)...),
		}
		for _, c := range app.Copies {
			status := statusWord(app, c)
			cd := copyDoc{
				Copy:             c.Copy,
				Role:             strOrNull(c.Role),
				Status:           status,
				Readiness:        strOrNull(readyWord(status, c)),
				Alive:            c.Alive,
				StartedAt:        timeOrNull(c.StartedAt),
				Crashes:          crashesShown(app, c),
				MemoryBytes:      bytesOrNull(c.MemoryBytes),
				Outdated:         c.Outdated,
				SkipDrain:        c.SkipDrain,
				DrainEndsAt:      timeOrNull(c.DrainEndsAt),
				LastExitReason:   strOrNull(c.LastExitReason),
				PageProblem:      strOrNull(c.PageProblem),
				PageWaitingSince: timeOrNull(c.PageWaitingSince),
			}
			// A record keeps the last PID after the process is gone, and
			// the number may have been reused.
			if c.Alive {
				pid := c.PID
				cd.PID = &pid
			}
			if c.Sessions != nil {
				n := *c.Sessions
				cd.Sessions = &n
			}
			ad.Copies = append(ad.Copies, cd)
		}
		doc.Apps = append(doc.Apps, ad)
	}
	return doc
}

// notRunningJSON builds the document for a shinyctl-api that is not running:
// the registered apps with the same shape, and null or [] for what only a
// running shinyctl-api can see. Copies and the load balancer may still run
// (they outlive a crashed shinyctl-api), so those are not inspected, not
// none. An unreadable registry is an error: serve would refuse to start with
// it, so "down, no apps" would be false.
func notRunningJSON(registryPath string, now time.Time) (statusDoc, error) {
	reg, err := registry.Open(registryPath)
	if err != nil {
		return statusDoc{}, err
	}
	specs := reg.All()
	slices.SortFunc(specs, specOrder)

	doc := statusDoc{
		Schema: 1,
		Time:   now.UTC().Format(time.RFC3339),
		API:    apiDoc{Running: false},
		Apps:   []appDoc{},
		Notes:  []string{notRunningLine(registryPath)},
	}
	for _, spec := range specs {
		doc.Apps = append(doc.Apps, notRunningApp(spec, reg.IsStopped(spec.AppStateFile)))
	}
	return doc, nil
}

func notRunningApp(spec manifest.AppSpec, stopped bool) appDoc {
	return appDoc{
		Name:           spec.AppName,
		Group:          strOrNull(spec.AppGroup),
		Host:           spec.EffectiveHost(),
		Port:           spec.Port,
		Stopped:        stopped,
		Replicas:       spec.EffectiveReplicas(),
		WarmReplicas:   spec.EffectiveWarm(),
		RestartPolicy:  strOrNull(string(spec.RestartPolicy.Effective())),
		MaxMemoryBytes: bytesOrNull(spec.EffectiveMaxMemory()),
		Copies:         []copyDoc{},
		Notes:          []string{},
	}
}

// writeStatusJSON writes the document pretty-printed, with a trailing newline
// and no HTML escaping.
func writeStatusJSON(out io.Writer, doc statusDoc) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(doc)
}
