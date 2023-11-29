#! /bin/bash

# Start the postgres database.
sudo docker-compose up -d

# Start a virtual env for the app.
python3 -m virtualenv venv
source venv/bin/activate

# Install the dependencies.
pip3 install -r requirements.txt

# Set the environment variable for the app to run correctly.
export PYTHON_PATH=./venv/lib/python3.10/site-packages/django

