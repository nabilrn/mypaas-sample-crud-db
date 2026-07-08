package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type api struct {
	db     *pgxpool.Pool
	logger *slog.Logger
}

type todo struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Done      bool      `json:"done"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type createTodoRequest struct {
	Title string `json:"title"`
}

type patchTodoRequest struct {
	Title *string `json:"title"`
	Done  *bool   `json:"done"`
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openDB(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		logger.Error("database connection failed", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	app := &api{db: db, logger: logger}
	if err := app.migrate(ctx); err != nil {
		logger.Error("database migration failed", "error", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", app.index)
	mux.HandleFunc("GET /health", app.health)
	mux.HandleFunc("GET /todos", app.listTodos)
	mux.HandleFunc("POST /todos", app.createTodo)
	mux.HandleFunc("GET /todos/{id}", app.getTodo)
	mux.HandleFunc("PATCH /todos/{id}", app.patchTodo)
	mux.HandleFunc("DELETE /todos/{id}", app.deleteTodo)

	port := getenv("APP_PORT", "8080")
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           app.recover(app.log(app.jsonOnly(mux))),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("server listening", "addr", server.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown failed", "error", err)
		os.Exit(1)
	}
}

func openDB(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	if databaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}

	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 4

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			return pool, nil
		}
		if time.Now().After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("ping database: %w", err)
		}
		time.Sleep(1 * time.Second)
	}
}

func (a *api) migrate(ctx context.Context) error {
	const query = `
CREATE TABLE IF NOT EXISTS todos (
	id BIGSERIAL PRIMARY KEY,
	title TEXT NOT NULL CHECK (length(trim(title)) > 0),
	done BOOLEAN NOT NULL DEFAULT false,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`

	_, err := a.db.Exec(ctx, query)
	return err
}

func (a *api) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(indexHTML))
}

func (a *api) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := a.db.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "DB_UNAVAILABLE", "database is not ready")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]string{
			"status": "ok",
		},
	})
}

func (a *api) listTodos(w http.ResponseWriter, r *http.Request) {
	const query = `
SELECT id, title, done, created_at, updated_at
FROM todos
ORDER BY id DESC;`

	rows, err := a.db.Query(r.Context(), query)
	if err != nil {
		a.logger.Error("list todos failed", "error", err)
		writeError(w, http.StatusInternalServerError, "LIST_TODOS_FAILED", "failed to list todos")
		return
	}
	defer rows.Close()

	todos := make([]todo, 0)
	for rows.Next() {
		item, err := scanTodo(rows)
		if err != nil {
			a.logger.Error("scan todo failed", "error", err)
			writeError(w, http.StatusInternalServerError, "LIST_TODOS_FAILED", "failed to list todos")
			return
		}
		todos = append(todos, item)
	}
	if err := rows.Err(); err != nil {
		a.logger.Error("iterate todos failed", "error", err)
		writeError(w, http.StatusInternalServerError, "LIST_TODOS_FAILED", "failed to list todos")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": todos})
}

func (a *api) createTodo(w http.ResponseWriter, r *http.Request) {
	var req createTodoRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	title := strings.TrimSpace(req.Title)
	if title == "" {
		writeError(w, http.StatusBadRequest, "INVALID_TITLE", "title is required")
		return
	}

	const query = `
INSERT INTO todos (title)
VALUES ($1)
RETURNING id, title, done, created_at, updated_at;`

	item, err := queryTodo(r.Context(), a.db, query, title)
	if err != nil {
		a.logger.Error("create todo failed", "error", err)
		writeError(w, http.StatusInternalServerError, "CREATE_TODO_FAILED", "failed to create todo")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{"data": item})
}

func (a *api) getTodo(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}

	const query = `
SELECT id, title, done, created_at, updated_at
FROM todos
WHERE id = $1;`

	item, err := queryTodo(r.Context(), a.db, query, id)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "TODO_NOT_FOUND", "todo not found")
		return
	}
	if err != nil {
		a.logger.Error("get todo failed", "error", err, "id", id)
		writeError(w, http.StatusInternalServerError, "GET_TODO_FAILED", "failed to get todo")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": item})
}

func (a *api) patchTodo(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}

	var req patchTodoRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if req.Title == nil && req.Done == nil {
		writeError(w, http.StatusBadRequest, "EMPTY_PATCH", "title or done is required")
		return
	}

	var titleArg any
	if req.Title != nil {
		trimmed := strings.TrimSpace(*req.Title)
		if trimmed == "" {
			writeError(w, http.StatusBadRequest, "INVALID_TITLE", "title cannot be empty")
			return
		}
		titleArg = trimmed
	}

	var doneArg any
	if req.Done != nil {
		doneArg = *req.Done
	}

	const query = `
UPDATE todos
SET
	title = COALESCE($2, title),
	done = COALESCE($3, done),
	updated_at = now()
WHERE id = $1
RETURNING id, title, done, created_at, updated_at;`

	item, err := queryTodo(r.Context(), a.db, query, id, titleArg, doneArg)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "TODO_NOT_FOUND", "todo not found")
		return
	}
	if err != nil {
		a.logger.Error("patch todo failed", "error", err, "id", id)
		writeError(w, http.StatusInternalServerError, "PATCH_TODO_FAILED", "failed to patch todo")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": item})
}

func (a *api) deleteTodo(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}

	tag, err := a.db.Exec(r.Context(), "DELETE FROM todos WHERE id = $1", id)
	if err != nil {
		a.logger.Error("delete todo failed", "error", err, "id", id)
		writeError(w, http.StatusInternalServerError, "DELETE_TODO_FAILED", "failed to delete todo")
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "TODO_NOT_FOUND", "todo not found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func queryTodo(ctx context.Context, db *pgxpool.Pool, query string, args ...any) (todo, error) {
	var item todo
	err := db.QueryRow(ctx, query, args...).Scan(
		&item.ID,
		&item.Title,
		&item.Done,
		&item.CreatedAt,
		&item.UpdatedAt,
	)
	return item, err
}

type todoScanner interface {
	Scan(dest ...any) error
}

func scanTodo(row todoScanner) (todo, error) {
	var item todo
	err := row.Scan(&item.ID, &item.Title, &item.Done, &item.CreatedAt, &item.UpdatedAt)
	return item, err
}

func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "INVALID_ID", "id must be a positive integer")
		return 0, false
	}
	return id, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON", "request body must be valid json")
		return false
	}
	return true
}

func (a *api) jsonOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodPatch {
			contentType := r.Header.Get("Content-Type")
			if !strings.HasPrefix(contentType, "application/json") {
				writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "content type must be application/json")
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func (a *api) log(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		a.logger.Info("request completed",
			"method", r.Method,
			"path", r.URL.Path,
			"durationMs", time.Since(start).Milliseconds(),
		)
	})
}

func (a *api) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				a.logger.Error("panic recovered", "panic", value)
				writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
			}
		}()

		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code string, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

func getenv(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

const indexHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Dummy Todos Cikiwir</title>
  <style>
    :root {
      color-scheme: light;
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      background: #eef2f7;
      color: #111827;
    }

    * {
      box-sizing: border-box;
    }

    body {
      margin: 0;
      min-height: 100vh;
      background:
        linear-gradient(180deg, #ffffff 0, rgba(255, 255, 255, 0) 300px),
        #eef2f7;
    }

    button, input {
      font: inherit;
    }

    button {
      border: 0;
      cursor: pointer;
    }

    .page {
      width: min(760px, calc(100% - 32px));
      margin: 0 auto;
      padding: 48px 0 42px;
    }

    .topbar {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 16px;
      margin-bottom: 18px;
      padding-bottom: 18px;
      border-bottom: 1px solid #d9dee7;
    }

    .brand {
      display: flex;
      flex-direction: column;
      gap: 4px;
    }

    h1 {
      margin: 0;
      font-size: 32px;
      line-height: 1.1;
      letter-spacing: 0;
      font-weight: 800;
    }

    .meta {
      color: #667085;
      font-size: 13px;
      font-weight: 600;
    }

    .status {
      display: inline-flex;
      align-items: center;
      gap: 8px;
      min-height: 36px;
      padding: 0 12px;
      border: 1px solid #d9dee7;
      border-radius: 8px;
      background: #ffffff;
      color: #475467;
      font-size: 14px;
      white-space: nowrap;
      box-shadow: 0 1px 2px rgba(16, 24, 40, 0.04);
    }

    .dot {
      width: 8px;
      height: 8px;
      border-radius: 999px;
      background: #f59e0b;
    }

    .dot.ok {
      background: #10b981;
    }

    .dot.bad {
      background: #ef4444;
    }

    .toolbar {
      display: grid;
      grid-template-columns: minmax(0, 1fr) auto;
      gap: 8px;
      margin-bottom: 14px;
      padding: 8px;
      border: 1px solid #d9dee7;
      border-radius: 8px;
      background: #ffffff;
      box-shadow: 0 12px 32px rgba(16, 24, 40, 0.07);
    }

    .input {
      width: 100%;
      height: 42px;
      padding: 0 14px;
      border: 1px solid transparent;
      border-radius: 8px;
      background: #f8fafc;
      color: #111827;
      outline: none;
    }

    .input:focus {
      border-color: #0f766e;
      box-shadow: 0 0 0 3px rgba(15, 118, 110, 0.12);
    }

    .button {
      height: 42px;
      min-width: 96px;
      padding: 0 16px;
      border-radius: 8px;
      background: #0f766e;
      color: #fff;
      font-weight: 700;
    }

    .button:hover {
      background: #115e59;
    }

    .button.secondary {
      min-width: 44px;
      background: #e9eef5;
      color: #344054;
    }

    .button.secondary:hover {
      background: #dce3ed;
    }

    .button.danger {
      min-width: 44px;
      background: #fee2e2;
      color: #b42318;
    }

    .button.danger:hover {
      background: #fecaca;
    }

    .list {
      display: grid;
      gap: 8px;
    }

    .todo {
      display: grid;
      grid-template-columns: auto minmax(0, 1fr) auto;
      align-items: center;
      gap: 12px;
      min-height: 62px;
      padding: 12px;
      border: 1px solid #eaecf0;
      border-radius: 8px;
      background: #ffffff;
      box-shadow: 0 1px 2px rgba(16, 24, 40, 0.035);
    }

    .todo:hover {
      border-color: #cfd6e3;
    }

    .check {
      width: 22px;
      height: 22px;
      accent-color: #0f766e;
    }

    .title {
      min-width: 0;
      color: #111827;
      overflow-wrap: anywhere;
      line-height: 1.4;
    }

    .title.done {
      color: #667085;
      text-decoration: line-through;
    }

    .actions {
      display: flex;
      gap: 6px;
    }

    .actions .button {
      height: 38px;
      min-width: 64px;
      padding: 0 12px;
      font-size: 13px;
    }

    .empty {
      min-height: 180px;
      display: grid;
      place-items: center;
      border: 1px dashed #bcc6d5;
      border-radius: 8px;
      color: #667085;
      background: #ffffff;
    }

    .toast {
      position: fixed;
      left: 50%;
      bottom: 24px;
      transform: translateX(-50%);
      max-width: min(520px, calc(100% - 32px));
      padding: 12px 14px;
      border-radius: 8px;
      background: #17202a;
      color: #fff;
      box-shadow: 0 16px 40px rgba(16, 24, 40, 0.22);
      opacity: 0;
      pointer-events: none;
      transition: opacity 140ms ease;
    }

    .toast.show {
      opacity: 1;
    }

    @media (max-width: 640px) {
      .page {
        width: min(100% - 24px, 760px);
        padding-top: 24px;
      }

      .topbar {
        align-items: flex-start;
        flex-direction: column;
      }

      .toolbar {
        grid-template-columns: 1fr;
        padding: 8px;
      }

      .button {
        width: 100%;
      }

      .todo {
        grid-template-columns: auto minmax(0, 1fr);
      }

      .actions {
        grid-column: 1 / -1;
        justify-content: flex-end;
      }

      .actions .button {
        width: auto;
      }
    }
  </style>
</head>
<body>
  <main class="page">
    <section class="topbar">
      <div class="brand">
        <h1>Todos</h1>
        <div class="meta" id="count">Loading...</div>
      </div>
      <div class="status" aria-live="polite">
        <span class="dot" id="health-dot"></span>
        <span id="health-text">Checking</span>
      </div>
    </section>

    <form class="toolbar" id="create-form">
      <input class="input" id="title-input" name="title" autocomplete="off" maxlength="180" placeholder="New todo" required>
      <button class="button" type="submit">Add</button>
    </form>

    <section class="list" id="list"></section>
  </main>

  <div class="toast" id="toast" role="status" aria-live="polite"></div>

  <script>
    const list = document.querySelector("#list");
    const count = document.querySelector("#count");
    const form = document.querySelector("#create-form");
    const input = document.querySelector("#title-input");
    const toast = document.querySelector("#toast");
    const healthDot = document.querySelector("#health-dot");
    const healthText = document.querySelector("#health-text");

    let todos = [];
    let toastTimer = null;

    async function request(path, options = {}) {
      const response = await fetch(path, {
        headers: {
          "Content-Type": "application/json",
          ...(options.headers || {})
        },
        ...options
      });

      if (response.status === 204) {
        return null;
      }

      const body = await response.json();
      if (!response.ok) {
        const message = body.error?.message || "Request failed";
        throw new Error(message);
      }
      return body.data;
    }

    function showToast(message) {
      toast.textContent = message;
      toast.classList.add("show");
      clearTimeout(toastTimer);
      toastTimer = setTimeout(() => toast.classList.remove("show"), 2200);
    }

    function render() {
      count.textContent = todos.length === 1 ? "1 item" : todos.length + " items";

      if (todos.length === 0) {
        list.innerHTML = '<div class="empty">No todos yet</div>';
        return;
      }

      list.replaceChildren(...todos.map((todo) => {
        const row = document.createElement("article");
        row.className = "todo";

        const check = document.createElement("input");
        check.className = "check";
        check.type = "checkbox";
        check.checked = todo.done;
        check.ariaLabel = "Toggle " + todo.title;
        check.addEventListener("change", () => updateTodo(todo.id, { done: check.checked }));

        const title = document.createElement("div");
        title.className = "title" + (todo.done ? " done" : "");
        title.textContent = todo.title;

        const actions = document.createElement("div");
        actions.className = "actions";

        const edit = document.createElement("button");
        edit.className = "button secondary";
        edit.type = "button";
        edit.textContent = "Edit";
        edit.addEventListener("click", () => editTodo(todo));

        const remove = document.createElement("button");
        remove.className = "button danger";
        remove.type = "button";
        remove.textContent = "Delete";
        remove.addEventListener("click", () => deleteTodo(todo.id));

        actions.append(edit, remove);
        row.append(check, title, actions);
        return row;
      }));
    }

    async function loadTodos() {
      todos = await request("/todos");
      render();
    }

    async function checkHealth() {
      try {
        await request("/health", { headers: {} });
        healthDot.className = "dot ok";
        healthText.textContent = "Online";
      } catch (error) {
        healthDot.className = "dot bad";
        healthText.textContent = "Offline";
      }
    }

    async function createTodo(title) {
      const todo = await request("/todos", {
        method: "POST",
        body: JSON.stringify({ title })
      });
      todos = [todo, ...todos];
      render();
    }

    async function updateTodo(id, patch) {
      try {
        const updated = await request("/todos/" + id, {
          method: "PATCH",
          body: JSON.stringify(patch)
        });
        todos = todos.map((todo) => todo.id === id ? updated : todo);
        render();
      } catch (error) {
        showToast(error.message);
        await loadTodos();
      }
    }

    async function editTodo(todo) {
      const title = window.prompt("Todo title", todo.title);
      if (title === null) {
        return;
      }
      const trimmed = title.trim();
      if (!trimmed) {
        showToast("Title cannot be empty");
        return;
      }
      await updateTodo(todo.id, { title: trimmed });
    }

    async function deleteTodo(id) {
      try {
        await request("/todos/" + id, { method: "DELETE" });
        todos = todos.filter((todo) => todo.id !== id);
        render();
      } catch (error) {
        showToast(error.message);
      }
    }

    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      const title = input.value.trim();
      if (!title) {
        return;
      }

      try {
        await createTodo(title);
        input.value = "";
        input.focus();
      } catch (error) {
        showToast(error.message);
      }
    });

    Promise.all([checkHealth(), loadTodos()]).catch((error) => showToast(error.message));
    setInterval(checkHealth, 10000);
  </script>
</body>
</html>`
