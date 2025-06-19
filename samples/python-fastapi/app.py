# examples/python-fastapi/app.py
from fastapi import FastAPI
from pydantic import BaseModel

app = FastAPI()

class EchoRequest(BaseModel):
    msg: str

@app.get("/")
def read_root():
    return {"Hello": "World"}

@app.post("/echo")
def echo(data: EchoRequest):
    return data