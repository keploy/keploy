from sentence_transformers import SentenceTransformer
from flask import Flask, request, jsonify
import logging

# Configure logging
logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

app = Flask(__name__)

# Load the model once at startup
logger.info("Loading embedding model...")
model = SentenceTransformer('sentence-transformers/all-MiniLM-L6-v2')
logger.info("Model loaded successfully")

@app.route('/embed_text', methods=['POST'])
def embed_text():
    try:
        data = request.json
        if not data or 'text' not in data:
            return jsonify({"error": "No text provided"}), 400
            
        text = data['text']
        if not text.strip():
            return jsonify({"error": "Empty text provided"}), 400
            
        # Generate embedding
        embedding = model.encode(text).tolist()
        
        return jsonify({
            "embedding": embedding,
            "dimension": len(embedding),
            "model": "all-MiniLM-L6-v2"
        })
        
    except Exception as e:
        logger.error(f"Error generating embedding: {str(e)}")
        return jsonify({"error": str(e)}), 500

@app.route('/health', methods=['GET'])
def health():
    return jsonify({
        "status": "healthy",
        "model": "all-MiniLM-L6-v2",
        "service": "embedding-service"
    })

@app.route('/batch_embed', methods=['POST'])
def batch_embed():
    try:
        data = request.json
        if not data or 'texts' not in data:
            return jsonify({"error": "No texts provided"}), 400
            
        texts = data['texts']
        if not isinstance(texts, list) or len(texts) == 0:
            return jsonify({"error": "Texts must be a non-empty list"}), 400
            
        # Generate embeddings for all texts
        embeddings = model.encode(texts).tolist()
        
        return jsonify({
            "embeddings": embeddings,
            "count": len(embeddings),
            "dimension": len(embeddings[0]) if embeddings else 0
        })
        
    except Exception as e:
        logger.error(f"Error generating batch embeddings: {str(e)}")
        return jsonify({"error": str(e)}), 500

if __name__ == '__main__':
    print("Starting Keploy RAG Embedding Service on port 8080...")
    app.run(host='0.0.0.0', port=8080, debug=True)
