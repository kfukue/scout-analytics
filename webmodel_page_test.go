package main

import (
	"io/fs"
	"os/exec"
	"strings"
	"testing"
)

// The model's scores on the page: the Model 7d cell (app.js) and the column
// in index.html.

// TestAppModelNode runs testdata/app-model.test.js: the page's Model 7d cell
// (the medium runner probability, "top N%", "n.v.", a dash without a score).
func TestAppModelNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on the PATH")
	}
	out, err := exec.Command(node, "testdata/app-model.test.js").CombinedOutput()
	if err != nil {
		t.Fatalf("node testdata/app-model.test.js: %v: %s", err, out)
	}
	if !strings.HasPrefix(string(out), "ok: ") {
		t.Fatalf("node testdata/app-model.test.js: got %q, want \"ok: …\"", out)
	}
}

// TestIndexModelColumn: the page has the Model 7d header right after
// Perceptor, sortable, and its legend entry (both hidden by app.js when the
// website does not show scores).
func TestIndexModelColumn(t *testing.T) {
	b, err := fs.ReadFile(frontendFS, "frontend/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	perc := strings.Index(html, `<th scope="col">Perceptor</th>`)
	model := strings.Index(html, `<th scope="col" class="num model" data-sort="model"`)
	status := strings.Index(html, `<th scope="col">Status</th>`)
	if perc < 0 || model < perc || status < model {
		t.Fatalf("header order: Perceptor %d, Model %d, Status %d", perc, model, status)
	}
	if !strings.Contains(html, `id="legend-model"`) {
		t.Fatal("no legend entry for the Model column")
	}
}
