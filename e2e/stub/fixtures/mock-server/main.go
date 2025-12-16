package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

type User struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type Product struct {
	ID    int     `json:"id"`
	Name  string  `json:"name"`
	Price float64 `json:"price"`
}

var (
	users    = []User{}
	products = []Product{
		{ID: 1, Name: "Product A", Price: 99.99},
		{ID: 2, Name: "Product B", Price: 149.99},
	}
	nextUserID = 1
)

func main() {
	port := os.Getenv("MOCK_SERVER_PORT")
	if port == "" {
		port = "9000"
	}

	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/api/users", usersHandler)
	http.HandleFunc("/api/users/", userByIDHandler)
	http.HandleFunc("/api/products", productsHandler)
	http.HandleFunc("/api/products/", productByIDHandler)
	http.HandleFunc("/api/echo", echoHandler)
	http.HandleFunc("/api/time", timeHandler)
	http.HandleFunc("/api/search", searchHandler)

	addr := ":" + port
	log.Printf("Mock server starting on %s\n", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func usersHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(users)
	case http.MethodPost:
		var user User
		if err := json.NewDecoder(r.Body).Decode(&user); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		user.ID = nextUserID
		nextUserID++
		users = append(users, user)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(user)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func userByIDHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Extract ID from path
	idStr := r.URL.Path[len("/api/users/"):]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	for _, user := range users {
		if user.ID == id {
			json.NewEncoder(w).Encode(user)
			return
		}
	}

	http.Error(w, "User not found", http.StatusNotFound)
}

func productsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(products)
}

func productByIDHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	// Extract ID from path
	idStr := r.URL.Path[len("/api/products/"):]
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}

	for _, product := range products {
		if product.ID == id {
			json.NewEncoder(w).Encode(product)
			return
		}
	}

	http.Error(w, "Product not found", http.StatusNotFound)
}

func echoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	body := make(map[string]interface{})
	if r.Body != nil {
		json.NewDecoder(r.Body).Decode(&body)
	}

	response := map[string]interface{}{
		"method":  r.Method,
		"path":    r.URL.Path,
		"query":   r.URL.Query(),
		"headers": r.Header,
		"body":    body,
	}

	json.NewEncoder(w).Encode(response)
}

func timeHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"timestamp": time.Now().Unix(),
		"iso":       time.Now().Format(time.RFC3339),
	})
}

func searchHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	query := r.URL.Query().Get("q")
	results := []interface{}{}

	// Search users
	for _, user := range users {
		if query == "" || contains(user.Name, query) || contains(user.Email, query) {
			results = append(results, user)
		}
	}

	// Search products
	for _, product := range products {
		if query == "" || contains(product.Name, query) {
			results = append(results, product)
		}
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"query":   query,
		"results": results,
	})
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
