package enterprisebackup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBundleRejectsReasonlessGovernanceAlpha(t *testing.T) {
	root, manifest := testBundle(t)
	for i := range manifest.Tables {
		if manifest.Tables[i].Name == "governance_actions" {
			columns := make([]string, 0)
			for _, col := range manifest.Tables[i].Columns {
				if col != "reason" {
					columns = append(columns, col)
				}
			}
			manifest.Tables[i].Columns = columns
		}
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), root, "dep-test", "org-test"); err == nil {
		t.Fatal("reason-less governance alpha bundle was accepted")
	}
}

func TestGovernanceBackupRowRequiresOriginalReasonColumn(t *testing.T) {
	row := make(map[string]any)
	for _, col := range Columns("governance_actions") {
		row[col] = nil
	}
	row["reason"] = " original\n恢复\t "
	data, _ := json.Marshal(row)
	canonical, err := CanonicalRow("governance_actions", data)
	if err != nil {
		t.Fatal(err)
	}
	var restored map[string]any
	if json.Unmarshal(canonical, &restored) != nil || restored["reason"] != row["reason"] {
		t.Fatal("backup rewrote original rationale")
	}
	delete(row, "reason")
	data, _ = json.Marshal(row)
	if _, err := CanonicalRow("governance_actions", data); err == nil {
		t.Fatal("backup accepted row missing private reason")
	}
}
