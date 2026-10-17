package courier //nolint:testpackage // the configuration as the service loads it

import (
	"testing"

	sp "github.com/gopherex/schemapb/go/schemapb"
)

// The configuration reflects into the schema the SDK loads it with: a tag
// that does not parse fails here, not at startup.
func TestConfigSchema(t *testing.T) {
	t.Parallel()

	if _, err := sp.ReflectType[Config](sp.ID(sp.Namespace("courier"), sp.SchemaName("config"), sp.Ver(1, 0, 0))); err != nil {
		t.Fatal(err)
	}
}
