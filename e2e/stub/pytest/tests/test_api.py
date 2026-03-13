"""
Pytest E2E tests for Keploy stub record/replay functionality.

These tests make HTTP API calls that will be intercepted by Keploy's proxy.

Usage with Keploy stub:

Record mocks:
    keploy stub record -c "pytest tests/" -p ./stubs --name pytest-api-tests

Replay mocks:
    keploy stub replay -c "pytest tests/" -p ./stubs --name pytest-api-tests
"""
import pytest
import httpx


class TestHealthCheck:
    """Tests for health check endpoint."""
    
    def test_health_endpoint_returns_ok(self, client):
        """Test that health endpoint returns success."""
        response = client.get("/health")
        assert response.status_code == 200
        
        data = response.json()
        assert data["success"] is True
        assert data["message"] == "OK"
        assert "timestamp" in data


class TestUsersAPI:
    """Tests for users API endpoints."""
    
    def test_get_all_users(self, client):
        """Test getting all users."""
        response = client.get("/api/users")
        assert response.status_code == 200
        
        data = response.json()
        assert data["success"] is True
        assert isinstance(data["data"], list)
        assert len(data["data"]) > 0
        assert data["count"] == len(data["data"])
    
    def test_get_user_by_id(self, client):
        """Test getting a specific user by ID."""
        response = client.get("/api/users/1")
        assert response.status_code == 200
        
        data = response.json()
        assert data["success"] is True
        assert data["data"]["id"] == 1
        assert data["data"]["name"] == "Alice"
        assert data["data"]["email"] == "alice@example.com"
    
    def test_get_nonexistent_user_returns_404(self, client):
        """Test that requesting non-existent user returns 404."""
        response = client.get("/api/users/999")
        assert response.status_code == 404
        
        data = response.json()
        assert data["success"] is False
        assert "not found" in data["message"].lower()
    
    def test_create_new_user(self, client):
        """Test creating a new user."""
        new_user = {
            "name": "TestUser",
            "email": "testuser@example.com",
            "role": "user"
        }
        
        response = client.post("/api/users", json=new_user)
        assert response.status_code == 201
        
        data = response.json()
        assert data["success"] is True
        assert data["data"]["name"] == new_user["name"]
        assert data["data"]["email"] == new_user["email"]
        assert "id" in data["data"]
    
    def test_create_user_without_required_fields_fails(self, client):
        """Test that creating user without required fields fails."""
        response = client.post("/api/users", json={"name": "OnlyName"})
        assert response.status_code == 400
        
        data = response.json()
        assert data["success"] is False


class TestProductsAPI:
    """Tests for products API endpoints."""
    
    def test_get_all_products(self, client):
        """Test getting all products."""
        response = client.get("/api/products")
        assert response.status_code == 200
        
        data = response.json()
        assert data["success"] is True
        assert isinstance(data["data"], list)
        assert len(data["data"]) > 0
    
    def test_get_product_by_id(self, client):
        """Test getting a specific product by ID."""
        response = client.get("/api/products/1")
        assert response.status_code == 200
        
        data = response.json()
        assert data["success"] is True
        assert data["data"]["id"] == 1
        assert data["data"]["name"] == "Laptop"
        assert data["data"]["price"] == 999.99
    
    def test_get_nonexistent_product_returns_404(self, client):
        """Test that requesting non-existent product returns 404."""
        response = client.get("/api/products/999")
        assert response.status_code == 404
        
        data = response.json()
        assert data["success"] is False


class TestEchoAPI:
    """Tests for echo API endpoint."""
    
    def test_echo_get_request(self, client):
        """Test echo endpoint with GET request."""
        response = client.get("/api/echo", params={"foo": "bar", "baz": "qux"})
        assert response.status_code == 200
        
        data = response.json()
        assert data["success"] is True
        assert data["data"]["method"] == "GET"
        assert data["data"]["query"]["foo"] == "bar"
        assert data["data"]["query"]["baz"] == "qux"
    
    def test_echo_post_request_with_body(self, client):
        """Test echo endpoint with POST request and JSON body."""
        test_body = {"message": "Hello, Keploy!", "number": 42}
        
        response = client.post("/api/echo", json=test_body)
        assert response.status_code == 200
        
        data = response.json()
        assert data["success"] is True
        assert data["data"]["method"] == "POST"
        assert data["data"]["body"]["message"] == test_body["message"]
        assert data["data"]["body"]["number"] == test_body["number"]
    
    def test_echo_put_request(self, client):
        """Test echo endpoint with PUT request."""
        response = client.put("/api/echo", json={"update": "data"})
        assert response.status_code == 200
        
        data = response.json()
        assert data["data"]["method"] == "PUT"
    
    def test_echo_delete_request(self, client):
        """Test echo endpoint with DELETE request."""
        response = client.delete("/api/echo")
        assert response.status_code == 200
        
        data = response.json()
        assert data["data"]["method"] == "DELETE"


class TestSearchAPI:
    """Tests for search API endpoint."""
    
    def test_search_without_filters(self, client):
        """Test search endpoint without any filters."""
        response = client.get("/api/search")
        assert response.status_code == 200
        
        data = response.json()
        assert data["success"] is True
        assert isinstance(data["data"], list)
    
    def test_search_with_query(self, client):
        """Test search endpoint with query parameter."""
        response = client.get("/api/search", params={"q": "alice"})
        assert response.status_code == 200
        
        data = response.json()
        assert data["success"] is True
        assert data["query"] == "alice"
    
    def test_search_by_type_users(self, client):
        """Test search endpoint filtering by users type."""
        response = client.get("/api/search", params={"type": "users"})
        assert response.status_code == 200
        
        data = response.json()
        assert data["success"] is True
        for item in data["data"]:
            assert item["type"] == "user"
    
    def test_search_by_type_products(self, client):
        """Test search endpoint filtering by products type."""
        response = client.get("/api/search", params={"type": "products"})
        assert response.status_code == 200
        
        data = response.json()
        assert data["success"] is True
        for item in data["data"]:
            assert item["type"] == "product"
    
    def test_search_products_by_name(self, client):
        """Test searching products by name."""
        response = client.get("/api/search", params={"q": "laptop", "type": "products"})
        assert response.status_code == 200
        
        data = response.json()
        assert data["success"] is True
        assert len(data["data"]) > 0
        assert data["data"][0]["name"] == "Laptop"


class TestTimeAPI:
    """Tests for time API endpoint."""
    
    def test_get_current_time(self, client):
        """Test getting current time."""
        response = client.get("/api/time")
        assert response.status_code == 200
        
        data = response.json()
        assert data["success"] is True
        assert "timestamp" in data["data"]
        assert "iso" in data["data"]
        assert "unix" in data["data"]


class TestOrdersAPI:
    """Tests for orders API endpoint."""
    
    def test_get_all_orders(self, client):
        """Test getting all orders."""
        response = client.get("/api/orders")
        assert response.status_code == 200
        
        data = response.json()
        assert data["success"] is True
        assert isinstance(data["data"], list)
    
    def test_create_order(self, client):
        """Test creating a new order."""
        new_order = {
            "user_id": 1,
            "product_id": 2,
            "quantity": 3
        }
        
        response = client.post("/api/orders", json=new_order)
        assert response.status_code == 201
        
        data = response.json()
        assert data["success"] is True
        assert data["data"]["user_id"] == new_order["user_id"]
        assert data["data"]["product_id"] == new_order["product_id"]
        assert data["data"]["quantity"] == new_order["quantity"]
        assert data["data"]["status"] == "pending"


class TestWorkflows:
    """Tests for complete API workflows."""
    
    def test_user_workflow(self, client):
        """Test complete user workflow: check health, get users, create user, verify."""
        # Step 1: Health check
        health_response = client.get("/health")
        assert health_response.status_code == 200
        
        # Step 2: Get initial users
        users_response = client.get("/api/users")
        assert users_response.status_code == 200
        initial_users = users_response.json()["data"]
        
        # Step 3: Create new user
        new_user_data = {"name": "WorkflowUser", "email": "workflow@test.com"}
        create_response = client.post("/api/users", json=new_user_data)
        assert create_response.status_code == 201
        created_user = create_response.json()["data"]
        
        # Step 4: Verify user was created
        verify_response = client.get(f"/api/users/{created_user['id']}")
        assert verify_response.status_code == 200
        verified_user = verify_response.json()["data"]
        assert verified_user["name"] == new_user_data["name"]
        
        # Step 5: Search for new user
        search_response = client.get("/api/search", params={"q": "workflow", "type": "users"})
        assert search_response.status_code == 200
    
    def test_product_order_workflow(self, client):
        """Test product browsing and ordering workflow."""
        # Step 1: Browse products
        products_response = client.get("/api/products")
        assert products_response.status_code == 200
        products = products_response.json()["data"]
        
        # Step 2: Get details of first product
        product_id = products[0]["id"]
        product_response = client.get(f"/api/products/{product_id}")
        assert product_response.status_code == 200
        
        # Step 3: Search for products
        search_response = client.get("/api/search", params={"type": "products"})
        assert search_response.status_code == 200
        
        # Step 4: Create an order
        order_data = {"user_id": 1, "product_id": product_id, "quantity": 2}
        order_response = client.post("/api/orders", json=order_data)
        assert order_response.status_code == 201
        
        # Step 5: Verify orders
        orders_response = client.get("/api/orders")
        assert orders_response.status_code == 200


class TestEdgeCases:
    """Tests for edge cases and error handling."""
    
    def test_empty_search_query(self, client):
        """Test search with empty query."""
        response = client.get("/api/search", params={"q": ""})
        assert response.status_code == 200
    
    def test_special_characters_in_search(self, client):
        """Test search with special characters."""
        response = client.get("/api/search", params={"q": "test@#$%"})
        assert response.status_code == 200
    
    def test_large_request_body(self, client):
        """Test echo with large request body."""
        large_data = {"data": "x" * 10000, "items": list(range(100))}
        response = client.post("/api/echo", json=large_data)
        assert response.status_code == 200
    
    def test_concurrent_requests(self, client):
        """Test making multiple sequential requests."""
        for i in range(5):
            response = client.get("/api/users")
            assert response.status_code == 200
            
            response = client.get("/api/products")
            assert response.status_code == 200
