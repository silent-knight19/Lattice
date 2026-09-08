package report

import (
	"encoding/json"

	"github.com/silent-knight19/lattice/internal/security/audit"
)

// GenerateJSON serializes an AuditReport into formatted, deterministic JSON.
func GenerateJSON(report *audit.AuditReport) ([]byte, error) {
	return json.MarshalIndent(report, "", "  ")
}
