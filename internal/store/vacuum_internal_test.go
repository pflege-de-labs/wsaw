package store

import (
	"errors"
	"testing"
)

// TestAServerDialectIsNotVacuumed is Story 4.13, AC2: a store on PostgreSQL
// or MySQL is refused by name before anything touches its database, rather
// than reported as a vacuum that found nothing to reclaim. The store here has
// no database at all, which is the proof that nothing was attempted.
func TestAServerDialectIsNotVacuumed(t *testing.T) {
	t.Parallel()

	for _, d := range []dialect{postgresDialect{}, mysqlDialect{}} {
		s := &SQL{d: d}

		if s.SupportsVacuum() {
			t.Errorf("%s reports that it supports a vacuum", d.name())
		}

		if _, err := s.Vacuum(t.Context(), TriggerCLI, VacuumOptions{}); !errors.Is(err, ErrVacuumUnsupported) {
			t.Errorf("Vacuum on %s = %v, want ErrVacuumUnsupported", d.name(), err)
		}

		if _, err := s.PlanVacuum(t.Context(), VacuumOptions{}); !errors.Is(err, ErrVacuumUnsupported) {
			t.Errorf("PlanVacuum on %s = %v, want ErrVacuumUnsupported", d.name(), err)
		}
	}
}
