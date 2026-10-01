package tracker

import (
	"encoding/json"
	"testing"
)

func TestTrackerMetadataCurrent(t *testing.T) {
	tests := []struct {
		name    string
		local   string
		tracker map[string]interface{}
		want    bool
	}{
		{"no tracker metadata", `{"a":1}`, nil, true},
		{"matching key, extra local keys ignored", `{"labels":["x"],"note":"mine"}`, map[string]interface{}{"labels": []string{"x"}}, true},
		{"changed value", `{"labels":["x"]}`, map[string]interface{}{"labels": []string{"y"}}, false},
		{"missing key", `{"note":"mine"}`, map[string]interface{}{"labels": []string{}}, false},
		{"no local metadata", ``, map[string]interface{}{"labels": []string{}}, false},
		{"local not an object", `[1]`, map[string]interface{}{"labels": []string{}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := trackerMetadataCurrent(json.RawMessage(tt.local), tt.tracker); got != tt.want {
				t.Errorf("trackerMetadataCurrent = %v, want %v", got, tt.want)
			}
		})
	}
}
