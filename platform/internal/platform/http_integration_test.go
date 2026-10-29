package platform

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestDraftConflictAndValidatorUnavailable(t *testing.T) {
	f := newFixture(t)
	base := "/api/v1/projects/" + f.p
	resource := requireStatus(t, f.request("POST", base+"/scenarios", Object{"title": "Draft"}, nil), 201)
	path := base + "/scenarios/" + str(resource, "id") + "/versions"
	v := requireStatus(t, f.request("POST", path, Object{"definition": Object{}}, nil), 201)
	path += "/" + str(v, "id")
	requireStatus(t, f.request("PATCH", path, Object{"definition": Object{"title": "first"}}, map[string]string{"If-Match": `"1"`}), 200)
	requireStatus(t, f.request("PATCH", path, Object{"definition": Object{"title": "lost"}}, map[string]string{"If-Match": `"1"`}), 412)
	var source []byte
	if e := f.a.DB.Pool.QueryRow(context.Background(), "SELECT definition FROM versions WHERE id=$1", f.scenario).Scan(&source); e != nil {
		t.Fatal(e)
	}
	d := obj(source)
	d["dataset_item_id"] = resource["dataset_item_id"]
	requireStatus(t, f.request("PATCH", path, Object{"definition": d}, map[string]string{"If-Match": `"2"`}), 200)
	f.a.Validators = fakeValidators{fail(503, "DEPENDENCY_UNAVAILABLE", "offline")}
	requireStatus(t, f.request("POST", path+"/publish", nil, map[string]string{"If-Match": `"3"`}), 503)
	after := requireStatus(t, f.request("GET", path, nil, nil), 200)
	if str(after, "status") != "draft" || str(after, "revision") != "3" {
		t.Fatal("failed validator mutated draft", after)
	}
}
func TestSSEReplayCursorAndRevocation(t *testing.T) {
	f := newFixture(t)
	run := f.start(t)
	id := str(run, "run_id")
	initial := requireStatus(t, f.request("GET", "/api/v1/projects/"+f.p+"/runs/"+id, nil, nil), 200)
	e := f.a.DB.Tx(context.Background(), func(tx pgx.Tx) error { return aggregate(context.Background(), tx, id) })
	if e != nil {
		t.Fatal(e)
	}
	latest := requireStatus(t, f.request("GET", "/api/v1/projects/"+f.p+"/runs/"+id, nil, nil), 200)
	server := httptest.NewServer(f.http)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/v1/projects/"+f.p+"/runs/"+id+"/stream", nil)
	for _, c := range f.cookies {
		request.AddCookie(c)
	}
	request.Header.Set("Last-Event-ID", str(initial, "last_event_cursor"))
	response, e := http.DefaultClient.Do(request)
	if e != nil {
		t.Fatal(e)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal(response.Status)
	}
	scan := bufio.NewScanner(response.Body)
	seen := false
	for scan.Scan() {
		line := scan.Text()
		if strings.HasPrefix(line, "data: ") {
			var envelope Object
			if e = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &envelope); e != nil {
				t.Fatal(e)
			}
			if str(envelope, "cursor") != str(latest, "last_event_cursor") || str(sub(envelope, "payload"), "revision") != str(latest, "revision") {
				t.Fatal("SSE payload disagrees with snapshot", envelope, latest)
			}
			seen = true
			break
		}
	}
	if !seen {
		t.Fatal("no replay event")
	}
	requireStatus(t, f.request("POST", "/api/v1/auth/logout", nil, nil), 200)
	for scan.Scan() {
	}
	if ctx.Err() != nil {
		t.Fatal("logout did not close the existing SSE stream")
	}
}
