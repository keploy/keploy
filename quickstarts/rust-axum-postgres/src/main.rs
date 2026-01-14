use std::sync::Arc;
use axum::{routing::{get, post}, Router, extract::State, Json};
use serde::{Deserialize, Serialize};
use tokio_postgres::{NoTls, Client};
use tokio::sync::Mutex;

#[derive(Clone)]
struct AppState {
    db: Option<Arc<Client>>,
    mem: Option<Arc<Mutex<Vec<User>>>>,
}

#[derive(Serialize, Clone)]
struct User {
    id: i32,
    name: String,
}

#[derive(Deserialize)]
struct CreateUser {
    name: String,
}

async fn list_users(State(state): State<AppState>) -> Json<Vec<User>> {
    if let Some(db) = state.db {
        let rows = db.query("SELECT id, name FROM users ORDER BY id", &[]).await.unwrap();
        let mut users = Vec::with_capacity(rows.len());
        for r in rows {
            users.push(User { id: r.get(0), name: r.get(1) });
        }
        Json(users)
    } else {
        let mut result = Vec::new();
        if let Some(mem) = state.mem {
            let items = mem.lock().await;
            result.extend(items.iter().cloned());
        }
        Json(result)
    }
}

async fn create_user(State(state): State<AppState>, Json(payload): Json<CreateUser>) -> Json<User> {
    if let Some(db) = state.db {
        let row = db.query_one("INSERT INTO users(name) VALUES($1) RETURNING id, name", &[&payload.name]).await.unwrap();
        Json(User { id: row.get(0), name: row.get(1) })
    } else {
        if let Some(mem) = state.mem {
            let mut items = mem.lock().await;
            let id = (items.len() as i32) + 1;
            let user = User { id, name: payload.name };
            items.push(user.clone());
            Json(user)
        } else {
            Json(User { id: 0, name: "".to_string() })
        }
    }
}

#[tokio::main]
async fn main() {
    let db_url = std::env::var("DATABASE_URL").unwrap_or_else(|_| "postgres://postgres:postgres@localhost:5432/postgres".to_string());
    let mut state = AppState { db: None, mem: None };
    if let Ok((client, connection)) = tokio_postgres::connect(&db_url, NoTls).await {
        tokio::spawn(async move {
            let _ = connection.await;
        });
        let _ = client.execute("CREATE TABLE IF NOT EXISTS users (id SERIAL PRIMARY KEY, name TEXT NOT NULL)", &[]).await;
        state.db = Some(Arc::new(client));
    } else {
        state.mem = Some(Arc::new(Mutex::new(Vec::new())));
    }
    let app = Router::new()
        .route("/users", get(list_users).post(create_user))
        .with_state(state);
    let listener = tokio::net::TcpListener::bind(("0.0.0.0", 8080)).await.unwrap();
    axum::serve(listener, app).await.unwrap();
}
