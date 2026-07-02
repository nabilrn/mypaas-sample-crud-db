# Dummy CRUD DB App

Minimal Docker Compose app untuk test MyPaas deployment dengan database.

## Stack

- Go 1.22 REST API
- PostgreSQL 16
- Docker Compose

## Run local

```bash
cp .env.example .env
docker compose up --build
```

API jalan di:

```text
http://localhost:8080
```

UI jalan di:

```text
http://localhost:8080/
```

## Endpoints

```bash
curl http://localhost:8080/health

curl http://localhost:8080/todos

curl -X POST http://localhost:8080/todos \
  -H "Content-Type: application/json" \
  -d "{\"title\":\"deploy with database\"}"

curl -X PATCH http://localhost:8080/todos/1 \
  -H "Content-Type: application/json" \
  -d "{\"done\":true}"

curl -X DELETE http://localhost:8080/todos/1
```

## Notes for MyPaas

Project ini sengaja punya `docker-compose.yml`, jadi deploy detector harus memilih Compose mode.

Service yang diexpose:

- `app` port `8080`
- `db` internal PostgreSQL

Data PostgreSQL disimpan di volume Compose bernama `postgres_data`.
