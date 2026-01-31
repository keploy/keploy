"""
Simple Flask server that acts as an external API dependency.
This server is used to test Keploy's stub record/replay functionality with Pytest.
"""
import os
from datetime import datetime
from flask import Flask, request, jsonify

app = Flask(__name__)

# Sample data
users = {
    1: {"id": 1, "name": "Alice", "email": "alice@example.com", "role": "admin"},
    2: {"id": 2, "name": "Bob", "email": "bob@example.com", "role": "user"},
    3: {"id": 3, "name": "Charlie", "email": "charlie@example.com", "role": "user"},
}

products = {
    1: {"id": 1, "name": "Laptop", "price": 999.99, "stock": 50},
    2: {"id": 2, "name": "Phone", "price": 699.99, "stock": 100},
    3: {"id": 3, "name": "Tablet", "price": 449.99, "stock": 75},
}

next_user_id = 4
next_product_id = 4


@app.route("/health")
def health():
    return jsonify({"success": True, "message": "OK", "timestamp": datetime.utcnow().isoformat()})


@app.route("/api/users", methods=["GET", "POST"])
def users_endpoint():
    global next_user_id
    
    if request.method == "GET":
        user_list = list(users.values())
        return jsonify({"success": True, "data": user_list, "count": len(user_list)})
    
    if request.method == "POST":
        data = request.get_json()
        if not data or "name" not in data or "email" not in data:
            return jsonify({"success": False, "message": "Name and email are required"}), 400
        
        new_user = {
            "id": next_user_id,
            "name": data["name"],
            "email": data["email"],
            "role": data.get("role", "user")
        }
        users[next_user_id] = new_user
        next_user_id += 1
        
        return jsonify({"success": True, "data": new_user}), 201


@app.route("/api/users/<int:user_id>")
def user_endpoint(user_id):
    if user_id not in users:
        return jsonify({"success": False, "message": "User not found"}), 404
    
    return jsonify({"success": True, "data": users[user_id]})


@app.route("/api/products", methods=["GET"])
def products_endpoint():
    product_list = list(products.values())
    return jsonify({"success": True, "data": product_list, "count": len(product_list)})


@app.route("/api/products/<int:product_id>")
def product_endpoint(product_id):
    if product_id not in products:
        return jsonify({"success": False, "message": "Product not found"}), 404
    
    return jsonify({"success": True, "data": products[product_id]})


@app.route("/api/echo", methods=["GET", "POST", "PUT", "DELETE", "PATCH"])
def echo_endpoint():
    return jsonify({
        "success": True,
        "data": {
            "method": request.method,
            "path": request.path,
            "query": dict(request.args),
            "headers": dict(request.headers),
            "body": request.get_json(silent=True),
        }
    })


@app.route("/api/time")
def time_endpoint():
    now = datetime.utcnow()
    return jsonify({
        "success": True,
        "data": {
            "timestamp": int(now.timestamp() * 1000),
            "iso": now.isoformat(),
            "unix": int(now.timestamp()),
        }
    })


@app.route("/api/search")
def search_endpoint():
    query = request.args.get("q", "").lower()
    search_type = request.args.get("type")
    
    results = []
    
    if not search_type or search_type == "users":
        for user in users.values():
            if not query or query in user["name"].lower():
                results.append({**user, "type": "user"})
    
    if not search_type or search_type == "products":
        for product in products.values():
            if not query or query in product["name"].lower():
                results.append({**product, "type": "product"})
    
    return jsonify({
        "success": True,
        "data": results,
        "query": query if query else None,
        "count": len(results)
    })


@app.route("/api/orders", methods=["GET", "POST"])
def orders_endpoint():
    """Simple orders endpoint for testing complex workflows."""
    if request.method == "GET":
        return jsonify({
            "success": True,
            "data": [
                {"id": 1, "user_id": 1, "product_id": 1, "quantity": 2, "status": "completed"},
                {"id": 2, "user_id": 2, "product_id": 2, "quantity": 1, "status": "pending"},
            ]
        })
    
    if request.method == "POST":
        data = request.get_json()
        if not data:
            return jsonify({"success": False, "message": "Invalid request"}), 400
        
        new_order = {
            "id": 3,
            "user_id": data.get("user_id"),
            "product_id": data.get("product_id"),
            "quantity": data.get("quantity", 1),
            "status": "pending"
        }
        return jsonify({"success": True, "data": new_order}), 201


if __name__ == "__main__":
    port = int(os.environ.get("MOCK_SERVER_PORT", 9000))
    app.run(host="0.0.0.0", port=port, debug=False)
