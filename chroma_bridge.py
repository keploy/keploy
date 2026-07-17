#!/usr/bin/env python3
import chromadb
import json
import sys
import argparse

def create_collection(collection_name):
    """Create or get a collection"""
    try:
        client = chromadb.HttpClient(host="localhost", port=8000)
        collection = client.get_or_create_collection(collection_name)
        return {"status": "success", "message": f"Collection '{collection_name}' created/found"}
    except Exception as e:
        return {"status": "error", "message": str(e)}

def add_documents(collection_name, ids, documents, metadatas, embeddings):
    """Add documents to collection"""
    try:
        client = chromadb.HttpClient(host="localhost", port=8000)
        collection = client.get_collection(collection_name)
        collection.add(
            ids=ids,
            documents=documents,
            metadatas=metadatas,
            embeddings=embeddings
        )
        return {"status": "success", "message": f"Added {len(documents)} documents"}
    except Exception as e:
        return {"status": "error", "message": str(e)}

def query_collection(collection_name, query_embeddings, n_results):
    """Query collection with embeddings"""
    try:
        client = chromadb.HttpClient(host="localhost", port=8000)
        collection = client.get_collection(collection_name)
        results = collection.query(
            query_embeddings=query_embeddings,
            n_results=n_results
        )
        return {"status": "success", "results": results}
    except Exception as e:
        return {"status": "error", "message": str(e)}

def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('action', choices=['create', 'add', 'query'])
    parser.add_argument('--collection', required=True)
    parser.add_argument('--data', help='JSON data for the operation')
    
    args = parser.parse_args()
    
    if args.action == 'create':
        result = create_collection(args.collection)
    elif args.action == 'add':
        data = json.loads(args.data)
        result = add_documents(
            args.collection,
            data['ids'],
            data['documents'], 
            data['metadatas'],
            data['embeddings']
        )
    elif args.action == 'query':
        data = json.loads(args.data)
        result = query_collection(
            args.collection,
            data['query_embeddings'],
            data['n_results']
        )
    
    print(json.dumps(result))

if __name__ == "__main__":
    main()
