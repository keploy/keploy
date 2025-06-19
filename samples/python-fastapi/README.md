# Python FastAPI Sample for Keploy HTTP Parser Testing

This folder contains a minimal [FastAPI](https://fastapi.tiangolo.com/) application used to test Keploy's HTTP parser as part of the CI pipeline.

## Files

- `app.py`: The FastAPI application with basic HTTP endpoints.
- `requirements.txt`: Python dependencies required to run the app.

## Usage

1. **Install dependencies:**
   ```bash
   pip install -r requirements.txt
   ```

2. **Run the FastAPI app:**
   ```bash
   uvicorn app:app --host 0.0.0.0 --port 8080
   ```

3. **Test with Keploy:**
   - Use Keploy in record and test mode as described in the main repository documentation or in the CI workflow.

## Endpoints

- `GET /` — Returns a simple greeting.
- `POST /echo` — Echoes back the posted JSON data.

## Purpose

This sample app is used in the Keploy test pipeline to validate HTTP traffic capture and replay for Python/FastAPI applications.

---