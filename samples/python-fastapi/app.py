# examples/python-fastapi/app.py
from fastapi import FastAPI

app = FastAPI()

@app.get("/")
def read_root():
    return {"Hello": "World"}

@app.post("/echo")
def echo(data: dict):
    return data