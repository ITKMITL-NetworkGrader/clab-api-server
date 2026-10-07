package api

// NTG-224: the running topology document is the clab-topo-file label of the lab's newest
// container. A 1.5 s cache of that lookup sent the write after an apply to the pre-apply file,
// and an apply that removed the newest containers left an older file nobody rewrote, so the
// document kept listing deleted nodes.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	clabcore "github.com/srl-labs/containerlab/core"

	"github.com/srl-labs/clab-api-server/internal/clab"
	"github.com/srl-labs/clab-api-server/internal/models"
)

type docLab struct {
	router *gin.Engine
	dir    string
	newest string // what the stubbed lookup reports as the newest container's topo file
}

func newDocLab(t *testing.T, labName string) *docLab {
	t.Helper()
	gin.SetMode(gin.TestMode)
	currentUser, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}
	root := filepath.Join(t.TempDir(), "labs")
	setTestClabLabsRoot(t, root)
	lab := &docLab{dir: filepath.Join(root, currentUser.Username, labName)}
	if err := os.MkdirAll(lab.dir, 0o750); err != nil {
		t.Fatal(err)
	}

	previousLookup := lookupLabInfo
	lookupLabInfo = func(_ context.Context, _ string, name string) (*models.ClabContainerInfo, bool, error) {
		return &models.ClabContainerInfo{LabName: name, Owner: currentUser.Username, AbsLabPath: lab.newest}, true, nil
	}
	previousApply := applyLab
	t.Cleanup(func() {
		lookupLabInfo = previousLookup
		applyLab = previousApply
	})

	lab.router = gin.New()
	lab.router.Use(func(c *gin.Context) {
		c.Set("username", currentUser.Username)
		c.Next()
	})
	lab.router.GET("/labs/:labName/topology/yaml", GetRunningLabYamlHandler)
	lab.router.PUT("/labs/:labName/topology/yaml", PutRunningLabYamlHandler)
	lab.router.POST("/labs/:labName/apply", ApplyTopologyHandler)
	return lab
}

func (l *docLab) file(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(l.dir, name)
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	return path
}

func (l *docLab) do(method, target, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	l.router.ServeHTTP(recorder, request)
	return recorder
}

func read(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func TestTopologyYamlReadFollowsNewestContainerAtOnce(t *testing.T) {
	lab := newDocLab(t, "ntg224-read")
	f0 := lab.file(t, "f0.clab.yml", "name: f0\n")
	f1 := lab.file(t, "f1.clab.yml", "name: f1\n")

	lab.newest = f0
	if got := lab.do(http.MethodGet, "/labs/ntg224-read/topology/yaml", "").Body.String(); got != "name: f0\n" {
		t.Fatalf("first read = %q", got)
	}
	lab.newest = f1
	if got := lab.do(http.MethodGet, "/labs/ntg224-read/topology/yaml", "").Body.String(); got != "name: f1\n" {
		t.Fatalf("read right after the newest container changed = %q, want f1's content", got)
	}
}

func TestTopologyYamlWriteGoesToTheCurrentFile(t *testing.T) {
	lab := newDocLab(t, "ntg224-write")
	f0 := lab.file(t, "f0.clab.yml", "name: f0\n")
	f1 := lab.file(t, "f1.clab.yml", "name: f1\n")

	lab.newest = f0
	lab.do(http.MethodGet, "/labs/ntg224-write/topology/yaml", "")
	lab.newest = f1
	if code := lab.do(http.MethodPut, "/labs/ntg224-write/topology/yaml", "name: new\n").Code; code != http.StatusOK {
		t.Fatalf("PUT status %d", code)
	}
	if got := read(t, f1); got != "name: new\n" {
		t.Fatalf("f1 = %q, want the PUT body", got)
	}
	if got := read(t, f0); got != "name: f0\n" {
		t.Fatalf("f0 = %q, want it untouched", got)
	}
}

// A delete-only apply removes the newest containers, so the newest label falls back to an older
// file (f0). The apply itself must leave that file holding what was applied, with no PUT after it.
func applyRemovingNewest(t *testing.T, labName, query string) (lab *docLab, f0 string, code int) {
	t.Helper()
	lab = newDocLab(t, labName)
	f0 = lab.file(t, "f0.clab.yml", "nodes: six\n")
	f1 := lab.file(t, "f1.clab.yml", "nodes: six\n")
	f2 := lab.file(t, "f2.clab.yml", "nodes: four\n")
	lab.newest = f1
	applyLab = func(_ context.Context, opts clab.ApplyOptions) (*clabcore.ApplyResult, error) {
		if opts.TopoPath != f2 {
			t.Errorf("applied %q, want %q", opts.TopoPath, f2)
		}
		lab.newest = f0
		// The applied file changing after the apply started must not be what gets published.
		if err := os.WriteFile(f2, []byte("nodes: changed later\n"), 0o640); err != nil {
			t.Error(err)
		}
		return &clabcore.ApplyResult{}, nil
	}
	code = lab.do(http.MethodPost, "/labs/"+labName+"/apply?path=f2.clab.yml"+query, "").Code
	return lab, f0, code
}

func TestApplyRewritesTheFileTheNewestContainerNowPointsAt(t *testing.T) {
	_, f0, code := applyRemovingNewest(t, "ntg224-apply", "")
	if code != http.StatusOK {
		t.Fatalf("apply status %d", code)
	}
	if got := read(t, f0); got != "nodes: four\n" {
		t.Fatalf("f0 = %q, want the applied topology", got)
	}
}

func TestStreamedApplyRewritesTheFileToo(t *testing.T) {
	_, f0, code := applyRemovingNewest(t, "ntg224-stream", "&stream=true")
	if code != http.StatusOK {
		t.Fatalf("apply status %d", code)
	}
	if got := read(t, f0); got != "nodes: four\n" {
		t.Fatalf("f0 = %q, want the applied topology", got)
	}
}

// Guards: these pass before the fix as well.

func TestDryRunOrFailedApplyLeavesTheRunningFileAlone(t *testing.T) {
	_, f0, _ := applyRemovingNewest(t, "ntg224-dry", "&dryRun=true")
	if got := read(t, f0); got != "nodes: six\n" {
		t.Fatalf("dry run changed f0 to %q", got)
	}

	lab := newDocLab(t, "ntg224-fail")
	f0 = lab.file(t, "f0.clab.yml", "nodes: six\n")
	lab.file(t, "f2.clab.yml", "nodes: four\n")
	lab.newest = f0
	applyLab = func(context.Context, clab.ApplyOptions) (*clabcore.ApplyResult, error) {
		return nil, errors.New("boom")
	}
	if code := lab.do(http.MethodPost, "/labs/ntg224-fail/apply?path=f2.clab.yml", "").Code; code != http.StatusInternalServerError {
		t.Fatalf("failed apply status %d", code)
	}
	if got := read(t, f0); got != "nodes: six\n" {
		t.Fatalf("failed apply changed f0 to %q", got)
	}
}

func TestApplyOfTheRunningFileDoesNotRewriteIt(t *testing.T) {
	lab := newDocLab(t, "ntg224-same")
	f2 := lab.file(t, "f2.clab.yml", "nodes: four\n")
	lab.newest = f2
	before, err := os.Stat(f2)
	if err != nil {
		t.Fatal(err)
	}
	applyLab = func(context.Context, clab.ApplyOptions) (*clabcore.ApplyResult, error) {
		return &clabcore.ApplyResult{}, nil
	}
	if code := lab.do(http.MethodPost, "/labs/ntg224-same/apply?path=f2.clab.yml", "").Code; code != http.StatusOK {
		t.Fatalf("apply status %d", code)
	}
	after, err := os.Stat(f2)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("the applied file was rewritten onto itself")
	}
}

func TestApplyDoesNotPublishOutsideTheAppliedDirectory(t *testing.T) {
	lab := newDocLab(t, "ntg224-dirs")
	if err := os.MkdirAll(filepath.Join(lab.dir, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	other := lab.file(t, filepath.Join("sub", "f0.clab.yml"), "nodes: six\n")
	lab.file(t, "f2.clab.yml", "nodes: four\n")
	lab.newest = other
	applyLab = func(context.Context, clab.ApplyOptions) (*clabcore.ApplyResult, error) {
		return &clabcore.ApplyResult{}, nil
	}
	lab.do(http.MethodPost, "/labs/ntg224-dirs/apply?path=f2.clab.yml", "")
	if got := read(t, other); got != "nodes: six\n" {
		t.Fatalf("file in another directory changed to %q", got)
	}

	lab.newest = "https://example.test/topo.clab.yml"
	if code := lab.do(http.MethodPost, "/labs/ntg224-dirs/apply?path=f2.clab.yml", "").Code; code != http.StatusOK {
		t.Fatalf("apply with a remote topology label: status %d", code)
	}
}

func TestTopologyYamlWriteWaitsForTheLabOperation(t *testing.T) {
	lab := newDocLab(t, "ntg224-lock")
	f0 := lab.file(t, "f0.clab.yml", "name: f0\n")
	lab.newest = f0

	release, _, ok := labOperations.begin("ntg224-lock", "apply")
	if !ok {
		t.Fatal("could not begin operation")
	}
	done := make(chan int, 1)
	go func() {
		done <- lab.do(http.MethodPut, "/labs/ntg224-lock/topology/yaml", "name: new\n").Code
	}()
	time.Sleep(200 * time.Millisecond)
	if got := read(t, f0); got != "name: f0\n" {
		release()
		t.Fatalf("PUT wrote %q while the lab operation was running", got)
	}
	release()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("PUT status %d after release", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PUT did not finish after the operation released")
	}
	if got := read(t, f0); got != "name: new\n" {
		t.Fatalf("f0 = %q after the PUT", got)
	}
}

func TestTopologyYamlWriteGivesUpWithConflictAfterTheWait(t *testing.T) {
	lab := newDocLab(t, "ntg224-busy")
	f0 := lab.file(t, "f0.clab.yml", "name: f0\n")
	lab.newest = f0
	previousWait := topologyDocLockWait
	topologyDocLockWait = 100 * time.Millisecond
	t.Cleanup(func() { topologyDocLockWait = previousWait })

	release, _, ok := labOperations.begin("ntg224-busy", "apply")
	if !ok {
		t.Fatal("could not begin operation")
	}
	defer release()
	if code := lab.do(http.MethodPut, "/labs/ntg224-busy/topology/yaml", "name: new\n").Code; code != http.StatusConflict {
		t.Fatalf("PUT status %d, want 409 while the lab stays busy", code)
	}
	if got := read(t, f0); got != "name: f0\n" {
		t.Fatalf("f0 = %q, want it untouched", got)
	}
}
