package rules

import (
	"github.com/silent-knight19/lattice/internal/security/audit"
	"github.com/silent-knight19/lattice/internal/security/model"
)

// DefaultRules returns an instantiated slice of all active security audit rules.
func DefaultRules() []model.AuditRule {
	return []model.AuditRule{
		NewSec001Unsafe(),
		NewSec002Exec(),
		NewSec004Perms(),
		NewSec008Secrets(),
		NewSec009Random(),
		NewSec010Logging(),
		NewSec011Panic(),
		NewSec012Alloc(),
		NewDepAuditRule(),
		NewConfigAuditRule(),
	}
}

// RegisterAllDefaultRules registers all standard security rules with the provided audit engine.
func RegisterAllDefaultRules(engine *audit.Engine) error {
	for _, rule := range DefaultRules() {
		if err := engine.RegisterRule(rule); err != nil {
			return err
		}
	}
	return nil
}
