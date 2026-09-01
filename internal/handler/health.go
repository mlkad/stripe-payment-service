package handler

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// healthProbe is the part of the database the health endpoints need. Narrowing
// it here keeps the handler package independent of pgx and makes the readiness
// path trivial to exercise with a stub.
type healthProbe interface {
	HealthCheck(ctx context.Context) error
}

// readinessTimeout bounds the probe independently of the request. A health
// endpoint that blocks for the full statement timeout is useless to a load
// balancer, which will have given up long before.
const readinessTimeout = 2 * time.Second

type HealthHandler struct {
	db        healthProbe
	log       *slog.Logger
	version   string
	startedAt time.Time
}

func NewHealthHandler(db healthProbe, log *slog.Logger, version string) *HealthHandler {
	return &HealthHandler{db: db, log: log, version: version, startedAt: time.Now()}
}

type healthResponse struct {
	Status  string           `json:"status"`
	Version string           `json:"version"`
	Uptime  string           `json:"uptime"`
	Checks  map[string]check `json:"checks,omitempty"`
}

type check struct {
	Status    string  `json:"status"`
	LatencyMS float64 `json:"latency_ms"`
	Error     string  `json:"error,omitempty"`
}

// Live answers the liveness probe: is the process running? It deliberately does
// not touch the database. Restarting this container cannot fix a database
// outage, so making liveness depend on the database turns a database blip into
// a cluster-wide restart loop.
func (h *HealthHandler) Live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{
		Status:  "ok",
		Version: h.version,
		Uptime:  h.uptime(),
	})
}

// Ready answers the readiness probe: should this instance receive traffic? It
// checks the database, since every meaningful request needs it.
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()

	start := time.Now()
	err := h.db.HealthCheck(ctx)
	latency := float64(time.Since(start).Microseconds()) / 1000

	resp := healthResponse{
		Status:  "ok",
		Version: h.version,
		Uptime:  h.uptime(),
		Checks:  map[string]check{"database": {Status: "ok", LatencyMS: latency}},
	}
	status := http.StatusOK

	if err != nil {
		// The response carries a generic reason only. pgx connection errors
		// embed the database user, database name and host, and this endpoint is
		// routinely reachable from outside the cluster. The detail goes to the
		// log, where the operator who needs it can actually see it.
		resp.Status = "unavailable"
		resp.Checks["database"] = check{Status: "fail", LatencyMS: latency, Error: "unreachable"}
		status = http.StatusServiceUnavailable
		h.log.ErrorContext(ctx, "readiness check failed", slog.String("error", err.Error()))
	}
	writeJSON(w, status, resp)
}

func (h *HealthHandler) uptime() string {
	return time.Since(h.startedAt).Truncate(time.Second).String()
}
