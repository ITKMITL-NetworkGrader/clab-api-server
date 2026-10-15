package docs

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/srl-labs/clab-api-server/internal/models"
)

// NTG-260: the generated enum lagged behind the protocols the server accepts.
func TestSwaggerTerminalProtocolEnumListsEveryProtocol(t *testing.T) {
	b, err := os.ReadFile("swagger.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Definitions map[string]struct {
			Enum []string `json:"enum"`
		} `json:"definitions"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	enum := doc.Definitions["models.TerminalProtocol"].Enum
	for _, p := range []models.TerminalProtocol{
		models.TerminalProtocolSSH, models.TerminalProtocolShell, models.TerminalProtocolTelnet,
		models.TerminalProtocolConsole, models.TerminalProtocolConsoleControl,
	} {
		if !slices.Contains(enum, string(p)) {
			t.Errorf("swagger enum %v lacks %q", enum, p)
		}
	}
}
