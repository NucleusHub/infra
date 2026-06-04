# Nucleus

Personal NAS hub — self-hosted dashboard for your home server apps.

## Stack

- **Frontend**: Vue 3 + Vite + Tailwind CSS v4
- **Backend**: Express + Mongoose
- **Database**: MongoDB
- **Containerisation**: Docker Compose

## Running with Docker

```sh
docker compose up --build
```

Open [http://localhost:5173](http://localhost:5173).

## Running locally

```sh
# Terminal 1 — API server
npm run server

# Terminal 2 — Vite frontend
npm run dev
```

Requires a local MongoDB instance. Copy `.env.example` to `.env` and fill in your values.

## Environment variables

| Variable | Description |
|---|---|
| `MONGODB_URI` | MongoDB connection string |
| `PORT` | API server port (default 3000) |
| `VITE_TMDB_API_KEY` | TMDb API key for movie/show metadata — get one free at [themoviedb.org](https://www.themoviedb.org/settings/api) |
