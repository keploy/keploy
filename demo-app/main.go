package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gorilla/mux"
)

type User struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type Response struct {
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

var users = []User{
	{ID: 1, Name: "John Doe", Email: "john@example.com"},
	{ID: 2, Name: "Jane Smith", Email: "jane@example.com"},
}

func main() {
	r := mux.NewRouter()

	// Health check endpoint
	r.HandleFunc("/health", healthHandler).Methods("GET")
	
	// User endpoints
	r.HandleFunc("/users", getUsersHandler).Methods("GET")
	r.HandleFunc("/users/{id:[0-9]+}", getUserHandler).Methods("GET")
	r.HandleFunc("/users", createUserHandler).Methods("POST")

	fmt.Println("🚀 Demo server starting on http://localhost:8080")
	fmt.Println("Available endpoints:")
	fmt.Println("  GET  /health")
	fmt.Println("  GET  /users")
	fmt.Println("  GET  /users/{id}")
	fmt.Println("  POST /users")
	
	log.Fatal(http.ListenAndServe(":8080", r))
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	response := Response{
		Message: "Server is healthy",
		Data: map[string]interface{}{
			"timestamp": time.Now().Unix(),
			"status":    "ok",
		},
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func getUsersHandler(w http.ResponseWriter, r *http.Request) {
	response := Response{
		Message: "Users retrieved successfully",
		Data:    users,
	}
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func getUserHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	idStr := vars["id"]
	
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid user ID", http.StatusBadRequest)
		return
	}
	
	for _, user := range users {
		if user.ID == id {
			response := Response{
				Message: "User found",
				Data:    user,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(response)
			return
		}
	}
	
	http.Error(w, "User not found", http.StatusNotFound)
}

func createUserHandler(w http.ResponseWriter, r *http.Request) {
	var newUser User
	
	if err := json.NewDecoder(r.Body).Decode(&newUser); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	
	// Generate new ID
	newUser.ID = len(users) + 1
	users = append(users, newUser)
	
	response := Response{
		Message: "User created successfully",
		Data:    newUser,
	}
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(response)
}