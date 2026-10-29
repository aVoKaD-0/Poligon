package platform

import (
	"context"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

const projectJSON = "to_jsonb(p) || jsonb_build_object('revision',p.revision::text)"
const resourceJSON = "to_jsonb(t) || jsonb_build_object('revision',t.revision::text)"
const versionJSON = "to_jsonb(v) || jsonb_build_object('revision',v.revision::text)"

func title(o Object, k string) error {
	s := str(o, k)
	if len(strings.TrimSpace(s)) < 1 || len(s) > 200 {
		return invalid(k + ": ожидается 1–200 символов")
	}
	return nil
}
func (a *App) projects(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	if r.Method == "GET" {
		n, c, e := page(r)
		if e != nil {
			return nil, e
		}
		items, e := queryJSON(ctx, a.DB.Pool, "SELECT "+projectJSON+" FROM projects p JOIN project_members m ON p.id=m.project_id WHERE m.user_id=$1 AND p.id>$2 ORDER BY p.id LIMIT $3", actor(ctx).ID, c, n+1)
		return pageResult(items, n), e
	}
	o, e := body(w, r)
	if e != nil {
		return nil, e
	}
	if e = requireFields(o, []string{"name"}, "name"); e != nil {
		return nil, e
	}
	if e = title(o, "name"); e != nil {
		return nil, e
	}
	id := ID()
	e = a.DB.Tx(ctx, func(tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, "INSERT INTO projects(id,name) VALUES($1,$2)", id, str(o, "name")); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, "INSERT INTO project_members(project_id,user_id,role) VALUES($1,$2,'owner')", id, actor(ctx).ID); e != nil {
			return e
		}
		return audit(ctx, tx, id, actor(ctx).ID, "project.created", id)
	})
	if e != nil {
		return nil, e
	}
	v, e := oneJSON(ctx, a.DB.Pool, "SELECT "+projectJSON+" FROM projects p WHERE id=$1", id)
	if e == nil {
		etag(w, v)
		writeJSON(w, 201, v)
	}
	return nil, e
}
func (a *App) project(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := method(r, "GET", "PATCH"); e != nil {
		return nil, e
	}
	role := ""
	if r.Method == "PATCH" {
		role = "owner"
	}
	if e := a.authorize(r, role); e != nil {
		return nil, e
	}
	ctx := r.Context()
	p := r.PathValue("project_id")
	if r.Method == "PATCH" {
		o, e := body(w, r)
		if e != nil {
			return nil, e
		}
		if e = requireFields(o, []string{"name"}, "name"); e != nil {
			return nil, e
		}
		if e = title(o, "name"); e != nil {
			return nil, e
		}
		e = a.DB.Tx(ctx, func(tx pgx.Tx) error {
			v, e := oneJSON(ctx, tx, "SELECT "+projectJSON+" FROM projects p WHERE id=$1 FOR UPDATE", p)
			if e != nil {
				return e
			}
			if e = checkETag(r, revision(v)); e != nil {
				return e
			}
			_, e = tx.Exec(ctx, "UPDATE projects SET name=$2,revision=revision+1 WHERE id=$1", p, str(o, "name"))
			if e != nil {
				return e
			}
			return audit(ctx, tx, p, actor(ctx).ID, "project.updated", p)
		})
		if e != nil {
			return nil, e
		}
	}
	v, e := oneJSON(ctx, a.DB.Pool, "SELECT "+projectJSON+" FROM projects p WHERE id=$1", p)
	etag(w, v)
	return v, e
}
func (a *App) members(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := method(r, "GET", "POST", "PATCH", "DELETE"); e != nil {
		return nil, e
	}
	role := ""
	if r.Method != "GET" {
		role = "owner"
	}
	if e := a.authorize(r, role); e != nil {
		return nil, e
	}
	ctx := r.Context()
	p := r.PathValue("project_id")
	if r.Method == "GET" {
		n, c, e := page(r)
		if e != nil {
			return nil, e
		}
		v, e := queryJSON(ctx, a.DB.Pool, "SELECT jsonb_build_object('id',m.user_id,'user_id',m.user_id,'role',m.role,'name',u.name) FROM project_members m JOIN users u ON u.id=m.user_id WHERE project_id=$1 AND user_id>$2 ORDER BY user_id LIMIT $3", p, c, n+1)
		return pageResult(v, n), e
	}
	var o Object
	var e error
	u := r.PathValue("user_id")
	if r.Method != "DELETE" {
		o, e = body(w, r)
		if e != nil {
			return nil, e
		}
		fields := []string{"role"}
		if r.Method == "POST" {
			fields = append(fields, "user_id")
		}
		if e = requireFields(o, fields, "role"); e != nil {
			return nil, e
		}
		if r.Method == "POST" {
			u = str(o, "user_id")
		}
		if str(o, "role") != "owner" && str(o, "role") != "editor" && str(o, "role") != "viewer" {
			return nil, invalid("Недопустимая роль")
		}
	}
	if !validID(u) {
		return nil, invalid("Требуется UUID пользователя")
	}
	e = a.DB.Tx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if e := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE id=$1)", u).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			return notFound()
		}
		var old string
		_ = tx.QueryRow(ctx, "SELECT role FROM project_members WHERE project_id=$1 AND user_id=$2", p, u).Scan(&old)
		if r.Method == "POST" && old != "" {
			return conflict("Участник уже добавлен")
		}
		if r.Method != "POST" && old == "" {
			return notFound()
		}
		if old == "owner" && (r.Method == "DELETE" || str(o, "role") != "owner") {
			var count int
			if e := tx.QueryRow(ctx, "SELECT count(*) FROM project_members WHERE project_id=$1 AND role='owner'", p).Scan(&count); e != nil {
				return e
			}
			if count == 1 {
				return conflict("Нельзя удалить последнего owner")
			}
		}
		if r.Method == "DELETE" {
			_, e = tx.Exec(ctx, "DELETE FROM project_members WHERE project_id=$1 AND user_id=$2", p, u)
		} else {
			_, e = tx.Exec(ctx, "INSERT INTO project_members(project_id,user_id,role) VALUES($1,$2,$3) ON CONFLICT(project_id,user_id) DO UPDATE SET role=excluded.role", p, u, str(o, "role"))
		}
		if e != nil {
			return e
		}
		return audit(ctx, tx, p, actor(ctx).ID, "membership."+strings.ToLower(r.Method), u)
	})
	return Object{"user_id": u, "role": str(o, "role")}, e
}
func (a *App) credentials(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := method(r, "GET", "POST", "DELETE"); e != nil {
		return nil, e
	}
	if e := a.authorize(r, "owner"); e != nil {
		return nil, e
	}
	ctx := r.Context()
	p := r.PathValue("project_id")
	if r.Method == "GET" {
		n, c, e := page(r)
		if e != nil {
			return nil, e
		}
		v, e := queryJSON(ctx, a.DB.Pool, "SELECT jsonb_build_object('id',id,'project_id',project_id,'label',label,'created_at',created_at,'revoked_at',revoked_at) FROM credentials WHERE project_id=$1 AND id>$2 ORDER BY id LIMIT $3", p, c, n+1)
		return pageResult(v, n), e
	}
	if r.Method == "DELETE" {
		id, e := pk(r, "credential_id")
		if e != nil {
			return nil, e
		}
		e = a.DB.Tx(ctx, func(tx pgx.Tx) error {
			tag, e := tx.Exec(ctx, "UPDATE credentials SET revoked_at=coalesce(revoked_at,now()) WHERE project_id=$1 AND id=$2", p, id)
			if e != nil {
				return e
			}
			if tag.RowsAffected() == 0 {
				return notFound()
			}
			return audit(ctx, tx, p, actor(ctx).ID, "credential.revoked", id)
		})
		return Object{"id": id, "revoked": true}, e
	}
	o, e := body(w, r)
	if e != nil {
		return nil, e
	}
	if e = requireFields(o, []string{"label", "value"}, "label", "value"); e != nil {
		return nil, e
	}
	if e = title(o, "label"); e != nil {
		return nil, e
	}
	if len(str(o, "value")) < 1 || len(str(o, "value")) > 16384 {
		return nil, invalid("value: 1–16384 байт")
	}
	cipher, e := encrypt(a.Config.MasterKey, str(o, "value"))
	if e != nil {
		return nil, e
	}
	id := ID()
	e = a.DB.Tx(ctx, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, "INSERT INTO credentials(id,project_id,label,ciphertext) VALUES($1,$2,$3,$4)", id, p, str(o, "label"), cipher)
		if e != nil {
			return e
		}
		return audit(ctx, tx, p, actor(ctx).ID, "credential.created", id)
	})
	if e == nil {
		writeJSON(w, 201, Object{"id": id, "project_id": p, "label": str(o, "label")})
	}
	return nil, e
}
func resourceKind(r *http.Request) string {
	if strings.Contains(r.URL.Path, "/agents") {
		return "agent"
	}
	return "scenario"
}
func (a *App) resources(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := method(r, "GET", "POST", "PATCH"); e != nil {
		return nil, e
	}
	role := ""
	if r.Method != "GET" {
		role = "editor"
	}
	if e := a.authorize(r, role); e != nil {
		return nil, e
	}
	ctx := r.Context()
	p, k, id := r.PathValue("project_id"), resourceKind(r), r.PathValue("resource_id")
	if id != "" && !validID(id) {
		return nil, notFound()
	}
	if r.Method == "GET" {
		if id != "" {
			v, e := oneJSON(ctx, a.DB.Pool, "SELECT "+resourceJSON+" FROM resources t WHERE project_id=$1 AND kind=$2 AND id=$3", p, k, id)
			etag(w, v)
			return v, e
		}
		n, c, e := page(r)
		if e != nil {
			return nil, e
		}
		items, e := queryJSON(ctx, a.DB.Pool, "SELECT "+resourceJSON+" FROM resources t WHERE project_id=$1 AND kind=$2 AND id>$3 ORDER BY id LIMIT $4", p, k, c, n+1)
		return pageResult(items, n), e
	}
	o, e := body(w, r)
	if e != nil {
		return nil, e
	}
	if e = requireFields(o, []string{"title", "archived"}); e != nil {
		return nil, e
	}
	if _, ok := o["title"]; ok {
		if e = title(o, "title"); e != nil {
			return nil, e
		}
	}
	if _, ok := o["archived"]; ok {
		if _, ok = o["archived"].(bool); !ok {
			return nil, invalid("archived должен быть boolean")
		}
	}
	if r.Method == "POST" {
		if id != "" {
			return nil, notFound()
		}
		if e = title(o, "title"); e != nil {
			return nil, e
		}
		id = ID()
		e = a.DB.Tx(ctx, func(tx pgx.Tx) error {
			var dataset any
			if k == "scenario" {
				dataset = ID()
			}
			_, e := tx.Exec(ctx, "INSERT INTO resources(id,project_id,kind,title,dataset_item_id) VALUES($1,$2,$3,$4,$5)", id, p, k, str(o, "title"), dataset)
			if e != nil {
				return e
			}
			return audit(ctx, tx, p, actor(ctx).ID, k+".created", id)
		})
	} else {
		if len(o) == 0 {
			return nil, invalid("Пустой patch")
		}
		e = a.DB.Tx(ctx, func(tx pgx.Tx) error {
			v, e := oneJSON(ctx, tx, "SELECT "+resourceJSON+" FROM resources t WHERE project_id=$1 AND kind=$2 AND id=$3 FOR UPDATE", p, k, id)
			if e != nil {
				return e
			}
			if e = checkETag(r, revision(v)); e != nil {
				return e
			}
			name := str(v, "title")
			archived, _ := v["archived"].(bool)
			if s, ok := o["title"]; ok {
				name = s.(string)
			}
			if b, ok := o["archived"]; ok {
				archived = b.(bool)
			}
			_, e = tx.Exec(ctx, "UPDATE resources SET title=$2,archived=$3,revision=revision+1 WHERE id=$1", id, name, archived)
			if e != nil {
				return e
			}
			return audit(ctx, tx, p, actor(ctx).ID, k+".updated", id)
		})
	}
	if e != nil {
		return nil, e
	}
	v, e := oneJSON(ctx, a.DB.Pool, "SELECT "+resourceJSON+" FROM resources t WHERE id=$1", id)
	etag(w, v)
	if r.Method == "POST" {
		writeJSON(w, 201, v)
		return nil, e
	}
	return v, e
}
func (a *App) versions(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := method(r, "GET", "POST", "PATCH"); e != nil {
		return nil, e
	}
	role := ""
	if r.Method != "GET" {
		role = "editor"
	}
	if e := a.authorize(r, role); e != nil {
		return nil, e
	}
	ctx := r.Context()
	p, k, parent := r.PathValue("project_id"), resourceKind(r), r.PathValue("resource_id")
	if !validID(parent) {
		return nil, notFound()
	}
	if _, e := oneJSON(ctx, a.DB.Pool, "SELECT to_jsonb(t) FROM resources t WHERE project_id=$1 AND id=$2 AND kind=$3", p, parent, k); e != nil {
		return nil, e
	}
	id := r.PathValue("version_id")
	if id != "" && !validID(id) {
		return nil, notFound()
	}
	if r.Method == "GET" {
		if id != "" {
			v, e := a.version(ctx, p, parent, id)
			etag(w, v)
			return v, e
		}
		n, c, e := page(r)
		if e != nil {
			return nil, e
		}
		v, e := queryJSON(ctx, a.DB.Pool, "SELECT "+versionJSON+" FROM versions v WHERE project_id=$1 AND parent_id=$2 AND id>$3 ORDER BY id LIMIT $4", p, parent, c, n+1)
		return pageResult(v, n), e
	}
	o, e := body(w, r)
	if e != nil {
		return nil, e
	}
	if e = requireFields(o, []string{"definition"}, "definition"); e != nil {
		return nil, e
	}
	d := sub(o, "definition")
	if d == nil {
		return nil, invalid("definition должен быть объектом")
	}
	if r.Method == "POST" {
		if id != "" {
			return nil, notFound()
		}
		id = ID()
		e = a.DB.Tx(ctx, func(tx pgx.Tx) error {
			var archived bool
			if e := tx.QueryRow(ctx, "SELECT archived FROM resources WHERE id=$1", parent).Scan(&archived); e != nil {
				return e
			}
			if archived {
				return conflict("Ресурс архивирован")
			}
			_, e := tx.Exec(ctx, "INSERT INTO versions(id,project_id,parent_id,kind,version_number,status,definition) SELECT $1,$2,$3,$4,coalesce(max(version_number),0)+1,'draft',$5 FROM versions WHERE parent_id=$3", id, p, parent, k, encode(d))
			if e != nil {
				return e
			}
			return audit(ctx, tx, p, actor(ctx).ID, "version.created", id)
		})
	} else {
		e = a.DB.Tx(ctx, func(tx pgx.Tx) error {
			v, e := oneJSON(ctx, tx, "SELECT "+versionJSON+" FROM versions v WHERE project_id=$1 AND parent_id=$2 AND id=$3 FOR UPDATE", p, parent, id)
			if e != nil {
				return e
			}
			if str(v, "status") != "draft" {
				return conflict("Опубликованная версия неизменяема")
			}
			if e = checkETag(r, revision(v)); e != nil {
				return e
			}
			_, e = tx.Exec(ctx, "UPDATE versions SET definition=$2,revision=revision+1 WHERE id=$1", id, encode(d))
			if e != nil {
				return e
			}
			return audit(ctx, tx, p, actor(ctx).ID, "version.updated", id)
		})
	}
	if e != nil {
		return nil, e
	}
	v, e := a.version(ctx, p, parent, id)
	etag(w, v)
	if r.Method == "POST" {
		writeJSON(w, 201, v)
		return nil, e
	}
	return v, e
}
func (a *App) version(ctx context.Context, p, parent, id string) (Object, error) {
	return oneJSON(ctx, a.DB.Pool, "SELECT "+versionJSON+" FROM versions v WHERE project_id=$1 AND parent_id=$2 AND id=$3", p, parent, id)
}
func (a *App) published(ctx context.Context, p, id, kind string) (Object, error) {
	if !validID(id) {
		return nil, invalid(kind + "_version_id должен быть UUID")
	}
	return oneJSON(ctx, a.DB.Pool, "SELECT "+versionJSON+" FROM versions v WHERE id=$1 AND (project_id=$2 OR project_id IS NULL) AND kind=$3 AND status='published'", id, p, kind)
}
func (a *App) publish(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := a.authorize(r, "editor"); e != nil {
		return nil, e
	}
	ctx := r.Context()
	p, parent, id := r.PathValue("project_id"), r.PathValue("resource_id"), r.PathValue("version_id")
	if !validID(id) || !validID(parent) {
		return nil, notFound()
	}
	v, e := a.version(ctx, p, parent, id)
	if e != nil {
		return nil, e
	}
	if str(v, "kind") != resourceKind(r) {
		return nil, notFound()
	}
	if str(v, "status") != "draft" {
		return nil, conflict("Версия уже опубликована")
	}
	if e = checkETag(r, revision(v)); e != nil {
		return nil, e
	}
	d := sub(v, "definition")
	if e = ValidateDefinition(str(v, "kind"), d); e != nil {
		return nil, e
	}
	_, h, e := canonical(d)
	if e != nil {
		return nil, invalid("Некорректный JSON definition")
	}
	if str(v, "kind") == "agent" {
		if e = a.Config.ValidateEndpoint(ctx, str(d, "endpoint")); e != nil {
			return nil, e
		}
		if ref := str(d, "credential_ref"); ref != "" {
			if e = a.credentialValid(ctx, p, ref); e != nil {
				return nil, e
			}
		}
	} else {
		resource, e := oneJSON(ctx, a.DB.Pool, "SELECT to_jsonb(t) FROM resources t WHERE id=$1 AND project_id=$2", parent, p)
		if e != nil {
			return nil, e
		}
		if str(d, "dataset_item_id") != str(resource, "dataset_item_id") {
			return nil, invalid("dataset_item_id должен совпадать с каталогом сценария")
		}
		for _, k := range []string{"environment", "simulator"} {
			if _, e = a.published(ctx, p, str(d, k+"_version_id"), k); e != nil {
				return nil, e
			}
		}
		if a.Validators == nil {
			return nil, fail(503, "DEPENDENCY_UNAVAILABLE", "Validators не настроены")
		}
		if e = a.Validators.ValidateScenario(ctx, d); e != nil {
			return nil, e
		}
	}
	e = a.DB.Tx(ctx, func(tx pgx.Tx) error {
		tag, e := tx.Exec(ctx, "UPDATE versions SET status='published',definition_sha256=$4,published_at=now(),revision=revision+1 WHERE project_id=$1 AND id=$2 AND revision=$3 AND status='draft'", p, id, revision(v), h)
		if e != nil {
			return e
		}
		if tag.RowsAffected() != 1 {
			return conflict("Draft изменился во время validation")
		}
		return audit(ctx, tx, p, actor(ctx).ID, "version.published", id)
	})
	if e != nil {
		return nil, e
	}
	v, e = a.version(ctx, p, parent, id)
	etag(w, v)
	return v, e
}
func (a *App) credentialValid(ctx context.Context, p, id string) error {
	if !validID(id) {
		return invalid("Некорректный credential_ref")
	}
	var ok bool
	e := a.DB.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM credentials WHERE project_id=$1 AND id=$2 AND revoked_at IS NULL)", p, id).Scan(&ok)
	if e != nil {
		return e
	}
	if !ok {
		return invalid("Credential отсутствует или отозван")
	}
	return nil
}
func (a *App) catalog(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := a.authorize(r, ""); e != nil {
		return nil, e
	}
	p := r.PathValue("project_id")
	var k string
	for _, s := range []string{"environment", "evaluator", "simulator"} {
		if strings.Contains(r.URL.Path, "/"+s+"-versions") {
			k = s
		}
	}
	if id := r.PathValue("version_id"); id != "" {
		return a.published(r.Context(), p, id, k)
	}
	n, c, e := page(r)
	if e != nil {
		return nil, e
	}
	v, e := queryJSON(r.Context(), a.DB.Pool, "SELECT "+versionJSON+" FROM versions v WHERE (project_id=$1 OR project_id IS NULL) AND kind=$2 AND status='published' AND id>$3 ORDER BY id LIMIT $4", p, k, c, n+1)
	return pageResult(v, n), e
}
