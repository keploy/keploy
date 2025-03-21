# Vector Database Integration for Keploy RAG System

This package provides integration with vector databases for semantic search and similarity-based code retrieval in the Keploy RAG (Retrieval-Augmented Generation) system.

## Overview

The vector database integration enhances Keploy's existing fuzzy matching system by providing:

1. Semantic search capabilities using modern embedding models
2. More accurate similarity matching for complex data structures
3. Dynamic indexing of newly added or modified code
4. Efficient retrieval of context snippets

## Enhanced Matching Algorithm

The vector database integration uses a sophisticated matching algorithm that offers several improvements over traditional fuzzy matching:

### Multi-Factor Scoring

Rather than relying solely on vector distance, our scoring system considers multiple factors:

- **Vector Similarity (70%)**: The primary factor is semantic similarity based on embeddings
- **Content Relevance (15%)**: Considers keyword matches between query and metadata
- **Recency (5%)**: Gives preference to more recently created documents
- **Domain Specificity (10%)**: Provides a boost when the query matches domain-specific terms

### Hybrid Search

The system combines vector similarity with traditional fuzzy matching:

- Vector similarity provides semantic understanding
- Fuzzy matching catches exact or near-exact matches
- Weighted combination (70% vector, 30% fuzzy) gives the final score

### Semantic Chunking

Instead of treating documents as simple text, the system uses semantic chunking:

- Request and response parts are extracted as separate meaningful chunks
- Different chunking strategies are applied based on document type (HTTP, Redis, MongoDB, etc.)
- Large documents are split along semantic boundaries rather than arbitrary character limits

### Context-Aware Matching

The system maintains context from previous requests to improve matching:

- Tracks request history by connection ID
- Enhances new requests with context from previous operations
- Provides better matching for multi-step operations or stateful protocols

## Supported Vector Databases

Currently, the following vector databases are supported:

- [ChromaDB](https://www.trychroma.com/) - A lightweight, open-source vector database that can be run locally or in the cloud.

Future implementations may include:
- Pinecone
- FAISS
- Weaviate

## Supported Embedding Models

The following embedding models are supported:

- OpenAI's text-embedding-3-small - High-performance text embeddings from OpenAI

## Configuration

The vector database integration can be configured using environment variables:

| Environment Variable | Description | Default |
| --- | --- | --- |
| KEPLOY_VECTOR_DB_ENABLED | Enable or disable the vector database integration | false |
| KEPLOY_VECTOR_DB_TYPE | Type of vector database to use | chroma |
| KEPLOY_EMBEDDING_MODEL_TYPE | Type of embedding model to use | openai |
| KEPLOY_VECTOR_DB_MIN_SCORE | Minimum similarity score for a match | 0.6 |
| KEPLOY_VECTOR_DB_TOP_K | Number of top results to retrieve | 3 |
| KEPLOY_VECTOR_DB_FALLBACK_TO_FUZZY | Fall back to fuzzy matching if no matches are found | true |
| OPENAI_API_KEY | API key for OpenAI (required if using OpenAI embeddings) | |
| CHROMA_URL | URL of the ChromaDB instance | http://localhost:8000 |

## Using ChromaDB

To use ChromaDB:

1. Run ChromaDB as a container:
   ```
   docker run -p 8000:8000 chromadb/chroma
   ```

2. Set the required environment variables:
   ```
   KEPLOY_VECTOR_DB_ENABLED=true
   KEPLOY_VECTOR_DB_TYPE=chroma
   CHROMA_URL=http://localhost:8000
   OPENAI_API_KEY=your-openai-api-key
   ```

## Architecture

The integration consists of the following components:

- **VectorDBService** - Interface for interacting with vector databases
- **EmbeddingService** - Interface for generating embeddings
- **RAGService** - Service for indexing and retrieving context
- **Factory** - Factory for creating and managing RAG service components
- **IntegrationService** - Service for integrating with the existing fuzzy matching system

## Performance Optimizations

The matching algorithm includes several optimizations:

- **Adaptive Shingle Size**: Automatically adjusts shingle size based on document length
- **Batch Processing**: Processes embeddings in batches to minimize API calls
- **Result Caching**: Frequently accessed results are cached for faster retrieval
- **Asynchronous Indexing**: New documents are indexed in the background to avoid blocking
- **Context Expiration**: Connection contexts expire after 30 minutes of inactivity

## Development

To contribute to the vector database integration:

1. Implement the `VectorDBService` interface for a new vector database
2. Implement the `EmbeddingService` interface for a new embedding model
3. Update the `Factory` to support the new implementations
4. Write tests for the new implementations

## Testing

To run the tests:

```
KEPLOY_VECTOR_DB_ENABLED=true OPENAI_API_KEY=your-api-key CHROMA_URL=http://localhost:8000 go test -v ./pkg/service/vector/...
``` 