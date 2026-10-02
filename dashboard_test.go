package snout

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The dashboard ships beside the schema and can only drift from it: every
// relation a panel queries must be one Schema creates.
func TestDashboardQueriesOnlyShippedRelations(t *testing.T) {
	raw, err := os.ReadFile("dashboards/overview.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				RawSQL string `json:"rawSql"`
			} `json:"targets"`
		} `json:"panels"`
		Templating struct {
			List []struct {
				Name string `json:"name"`
			} `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Panels) == 0 {
		t.Fatal("no panels; the checks below would be vacuous")
	}
	rel := regexp.MustCompile(`snout\.([a-z_]+)`)
	for _, p := range d.Panels {
		for _, tg := range p.Targets {
			for _, m := range rel.FindAllStringSubmatch(tg.RawSQL, -1) {
				if !strings.Contains(Schema, "snout."+m[1]+" ") {
					t.Errorf("panel %q queries snout.%s, which Schema does not create", p.Title, m[1])
				}
			}
			if !strings.Contains(tg.RawSQL, "hostname = '$hostname'") {
				t.Errorf("panel %q does not filter on hostname", p.Title)
			}
		}
	}
	var names []string
	for _, v := range d.Templating.List {
		names = append(names, v.Name)
	}
	if strings.Join(names, ",") != "datasource,site,hostname" {
		t.Errorf("variables %v", names)
	}
}
