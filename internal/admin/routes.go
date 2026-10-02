package admin

import (
	"net/http"
)

// Route paths, relative to APIPrefix. Declared as constants so the route table, the tests,
// and the documentation cannot drift apart by typo.
const (
	RouteHealth    = "/health"
	RouteNode      = "/node"
	RouteConfig    = "/config"
	RouteMetrics   = "/metrics"
	RouteSession   = "/session"
	RouteEvents    = "/events"
	RouteEngine    = "/engine/stats"
	RouteLSMTree   = "/lsm/tree"
	RouteLSMState  = "/lsm/compaction"
	RouteLSMDo     = "/lsm/compact"
	RouteLSMFlush  = "/lsm/flush"
	RouteSSTables  = "/sstables"
	RouteSSTInspct = "/sstables/inspect"
	RouteSSTRaw    = "/sstables/raw"
	RouteWALSegs   = "/wal/segments"
	RouteWALDump   = "/wal/dump"
	RouteManifest  = "/manifest"
	RouteRaftStat  = "/raft/status"
	RouteRaftPeers = "/raft/peers"
	RouteRaftLog   = "/raft/log"
	RouteRaftTl    = "/raft/timeline"
	RouteRaftCamp  = "/raft/campaign"
	RouteRaftStep  = "/raft/stepdown"
	RouteKeys      = "/keys"
	RouteKeyGet    = "/keys/get"
	RouteKeyPut    = "/keys/put"
	RouteKeyDelete = "/keys/delete"
	RouteLabStart  = "/lab/workload/start"
	RouteLabStat   = "/lab/workload"
	RouteLabCrash  = "/lab/crash"
	RouteLabRecov  = "/lab/recover-report"
	RouteLabClean  = "/lab/cleanup-orphans"
	RouteDiag      = "/diagnostics"
	RouteConsole   = "/console/exec"
)

// Handlers bundles the endpoint implementations a Router needs.
//
// Every field may be nil; nil handlers are simply not registered, which keeps the router
// honest while allowing the daemon to expose a subset of the API before every endpoint is
// implemented (ADM-4 and ADM-5 land before the Phase C-H endpoints do).
type Handlers struct {
	Health  http.Handler
	Node    http.Handler
	Config  http.Handler
	Metrics http.Handler
	Session http.Handler
	Events  http.Handler

	EngineStats http.Handler
	LSMTree     http.Handler
	LSMState    http.Handler
	LSMCompact  http.Handler
	LSMFlush    http.Handler

	SSTables   http.Handler
	SSTInspect http.Handler
	SSTRaw     http.Handler

	WALSegments http.Handler
	WALDump     http.Handler
	Manifest    http.Handler

	RaftStatus   http.Handler
	RaftPeers    http.Handler
	RaftLog      http.Handler
	RaftTimeline http.Handler
	RaftCampaign http.Handler
	RaftStepdown http.Handler

	Keys      http.Handler
	KeyGet    http.Handler
	KeyPut    http.Handler
	KeyDelete http.Handler

	LabWorkloadStart http.Handler
	LabWorkloadStat  http.Handler
	LabCrash         http.Handler
	LabRecoverReport http.Handler
	LabCleanup       http.Handler

	Diagnostics http.Handler
	ConsoleExec http.Handler
}

// routePlan is the authoritative permission map for the admin API.
//
// This table is the security policy in one place. Each entry states the MINIMUM role able
// to invoke the route, chosen by blast radius rather than convenience:
//
//   - Read-only observation of engine state: PermissionRead.
//   - Mutating stored data or engine structure: PermissionWrite. A reader must not be able
//     to change what the database contains.
//   - Anything destructive, disruptive, or that leaves the trust boundary: PermissionAdmin.
//
// Two entries deserve their reasoning called out:
//
//   - /raft/log requires PermissionAdmin (not Read). The Raft log holds COMMITTED COMMANDS,
//     which are user key/value payloads. A "read" of it is a bulk data export.
//   - /wal/dump requires PermissionAdmin for the same reason: WAL records carry raw
//     key/value bytes.
//
// The RouteSpecs that follow are generated from this table so the policy and the routing
// cannot diverge.
var routePlan = map[string]RouteSpec{
	// --- Node & health (read-only) ---
	RouteHealth: {Method: http.MethodGet, Path: RouteHealth, Permission: PermissionRead,
		description: "liveness, readiness and recovery state"},

	RouteNode: {Method: http.MethodGet, Path: RouteNode, Permission: PermissionRead,
		description: "node identity, addresses, uptime"},

	RouteConfig: {Method: http.MethodGet, Path: RouteConfig, Permission: PermissionRead,
		description: "resolved configuration with sensitive values redacted"},

	RouteMetrics: {Method: http.MethodGet, Path: RouteMetrics, Permission: PermissionRead,
		description: "JSON telemetry snapshot"},

	// The session endpoint discloses the CSRF token, so it is the one route that must NOT
	// be reader-gated: the console fetches it before it knows any role, and its contents
	// are worthless without the Host/Origin/CSRF guards already in front of it.
	RouteSession: {Method: http.MethodGet, Path: RouteSession, Permission: PermissionRead,
		description: "issues the per-boot CSRF token; guarded by Host+Origin+CSRF"},

	// The event stream carries Raft term transitions and operational metrics. Treated as
	// admin-only (design-doc ADM-5.3) because it is the highest-value, side-effect-free
	// target for a drive-by attacker and requires no per-request authz to subscribe.
	RouteEvents: {Method: http.MethodGet, Path: RouteEvents, Permission: PermissionAdmin,
		description: "SSE stream of Raft transitions and metrics samples"},

	// --- Engine & storage ---
	RouteEngine: {Method: http.MethodGet, Path: RouteEngine, Permission: PermissionRead,
		description: "engine counters and lifecycle state"},

	RouteLSMTree: {Method: http.MethodGet, Path: RouteLSMTree, Permission: PermissionRead,
		description: "LSM level and file inventory"},

	RouteLSMState: {Method: http.MethodGet, Path: RouteLSMState, Permission: PermissionRead,
		description: "compaction worker status and per-level durations"},

	RouteLSMDo: {Method: http.MethodPost, Path: RouteLSMDo, Permission: PermissionWrite, Mutating: true,
		description: "trigger a compaction"},

	RouteLSMFlush: {Method: http.MethodPost, Path: RouteLSMFlush, Permission: PermissionWrite, Mutating: true,
		description: "trigger a memtable flush"},

	// --- Forensics ---
	RouteSSTables: {Method: http.MethodGet, Path: RouteSSTables, Permission: PermissionRead,
		description: "SSTable inventory"},

	RouteSSTInspct: {Method: http.MethodGet, Path: RouteSSTInspct, Permission: PermissionRead,
		description: "SSTable forensic report (reads a file from the data dir)"},

	RouteSSTRaw: {Method: http.MethodGet, Path: RouteSSTRaw, Permission: PermissionRead,
		description: "bounded raw byte range of an SSTable"},

	RouteWALSegs: {Method: http.MethodGet, Path: RouteWALSegs, Permission: PermissionRead,
		description: "WAL segment inventory"},

	// Admin-only: WAL records carry raw key/value payloads (bulk data export).
	RouteWALDump: {Method: http.MethodGet, Path: RouteWALDump, Permission: PermissionAdmin,
		description: "WAL record dump (contains user key/value data)"},

	RouteManifest: {Method: http.MethodGet, Path: RouteManifest, Permission: PermissionRead,
		description: "MANIFEST version edit history"},

	// --- Raft ---
	RouteRaftStat: {Method: http.MethodGet, Path: RouteRaftStat, Permission: PermissionRead,
		description: "raft role, term, leader, commit index"},

	RouteRaftPeers: {Method: http.MethodGet, Path: RouteRaftPeers, Permission: PermissionRead,
		description: "peer replication indices and lag"},

	// Admin-only: the raft log contains committed commands, i.e. user key/value payloads.
	RouteRaftLog: {Method: http.MethodGet, Path: RouteRaftLog, Permission: PermissionAdmin,
		description: "raft log entries (contains committed user commands)"},

	RouteRaftTl: {Method: http.MethodGet, Path: RouteRaftTl, Permission: PermissionRead,
		description: "term and role transition timeline"},

	RouteRaftCamp: {Method: http.MethodPost, Path: RouteRaftCamp, Permission: PermissionAdmin, Mutating: true,
		description: "force a leadership campaign (disruptive: can deny availability)"},

	RouteRaftStep: {Method: http.MethodPost, Path: RouteRaftStep, Permission: PermissionAdmin, Mutating: true,
		description: "force the leader to step down (disruptive: forces a new election)"},

	// --- Keys ---
	RouteKeys: {Method: http.MethodGet, Path: RouteKeys, Permission: PermissionRead,
		description: "prefix listing"},

	RouteKeyGet: {Method: http.MethodGet, Path: RouteKeyGet, Permission: PermissionRead,
		description: "single key lookup"},

	RouteKeyPut: {Method: http.MethodPost, Path: RouteKeyPut, Permission: PermissionWrite, Mutating: true,
		description: "write a key"},

	RouteKeyDelete: {Method: http.MethodPost, Path: RouteKeyDelete, Permission: PermissionWrite, Mutating: true,
		description: "delete a key"},

	// --- Lab ---
	RouteLabStart: {Method: http.MethodPost, Path: RouteLabStart, Permission: PermissionAdmin, Mutating: true,
		description: "start a synthetic workload"},

	RouteLabStat: {Method: http.MethodGet, Path: RouteLabStat, Permission: PermissionAdmin,
		description: "workload progress and latency series"},

	RouteLabCrash: {Method: http.MethodPost, Path: RouteLabCrash, Permission: PermissionAdmin, Mutating: true,
		description: "schedule SIGKILL of this node (destructive)"},

	RouteLabRecov: {Method: http.MethodPost, Path: RouteLabRecov, Permission: PermissionAdmin, Mutating: true,
		description: "recovery report from the last crash"},

	RouteLabClean: {Method: http.MethodPost, Path: RouteLabClean, Permission: PermissionAdmin, Mutating: true,
		description: "delete orphaned files (destructive: removes files from disk)"},

	// --- Exfiltration boundary ---
	RouteDiag: {Method: http.MethodGet, Path: RouteDiag, Permission: PermissionAdmin,
		description: "downloadable diagnostics bundle (leaves the trust boundary)"},

	// Arbitrary command execution: admin-only regardless of argument.
	RouteConsole: {Method: http.MethodPost, Path: RouteConsole, Permission: PermissionAdmin, Mutating: true,
		description: "execute one parsed console command (equivalent to shell access)"},
}

// BuildRoutes produces the RouteSpec list for the handlers that are actually implemented.
//
// A nil handler means the endpoint does not exist yet, so its route is simply absent and
// the router answers 404. That keeps the default-deny property intact while the API is
// built incrementally, instead of registering a stub that could later be mistaken for a
// working (and possibly unguarded) endpoint.
func BuildRoutes(h *Handlers) []RouteSpec {
	if h == nil {
		return nil
	}
	byPath := map[string]http.Handler{
		RouteHealth: h.Health, RouteNode: h.Node, RouteConfig: h.Config,
		RouteMetrics: h.Metrics, RouteSession: h.Session, RouteEvents: h.Events,

		RouteEngine: h.EngineStats, RouteLSMTree: h.LSMTree, RouteLSMState: h.LSMState,
		RouteLSMDo: h.LSMCompact, RouteLSMFlush: h.LSMFlush,

		RouteSSTables: h.SSTables, RouteSSTInspct: h.SSTInspect, RouteSSTRaw: h.SSTRaw,
		RouteWALSegs: h.WALSegments, RouteWALDump: h.WALDump, RouteManifest: h.Manifest,

		RouteRaftStat: h.RaftStatus, RouteRaftPeers: h.RaftPeers, RouteRaftLog: h.RaftLog,
		RouteRaftTl: h.RaftTimeline, RouteRaftCamp: h.RaftCampaign, RouteRaftStep: h.RaftStepdown,

		RouteKeys: h.Keys, RouteKeyGet: h.KeyGet, RouteKeyPut: h.KeyPut, RouteKeyDelete: h.KeyDelete,

		RouteLabStart: h.LabWorkloadStart, RouteLabStat: h.LabWorkloadStat,
		RouteLabCrash: h.LabCrash, RouteLabRecov: h.LabRecoverReport, RouteLabClean: h.LabCleanup,

		RouteDiag: h.Diagnostics, RouteConsole: h.ConsoleExec,
	}

	specs := make([]RouteSpec, 0, len(routePlan))
	for path, impl := range byPath {
		if impl == nil {
			continue
		}
		spec := routePlan[path]
		spec.Handler = impl
		specs = append(specs, spec)
	}
	return specs
}

// PlanFor returns the declared permission for a route path, and whether it exists in the
// plan. Used by tests and by the audit surface.
func PlanFor(path string) (Permission, bool) {
	spec, ok := routePlan[path]
	if !ok {
		return 0, false
	}
	return spec.Permission, true
}

// AllPlannedRoutes returns every route path in the plan, including unimplemented ones.
func AllPlannedRoutes() []string {
	out := make([]string, 0, len(routePlan))
	for p := range routePlan {
		out = append(out, p)
	}
	return out
}
