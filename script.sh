#!/bin/bash

# we are in ~/keploy

#start the server
cd ~/samples-go/grpc/server/ && go run server.go &

#extract the api urls
cd ~/keploy/keploy/ && python3 ../extractUrls.py

cd ~/keploy && go test -coverprofile=coverage.out -coverpkg=./... -exec "sudo -E env 'PATH=$PATH'"  &

sleep 40

cd ~/keploy

#make api calls
file="./apiUrl.txt"
while read line;
do
  curl --request GET --url $line
    sleep 4
done<$file

rm apiUrl.txt

#match the mocks
python3 mockMatching.py

sudo rm -r ~/keploy/keployTest990

#kill keploy's server
PID=$(sudo lsof -i:16789 | sed "2q;d" | awk '{print $2}')
if [[ "$PID" != "" ]]; then
  sudo kill $PID
fi

#kill client process
PID=$(sudo lsof -i:8080 | sed "2q;d" | awk '{print $2}')
if [[ "$PID" != "" ]]; then
  sudo kill $PID
fi

#kill server process
PID=$(sudo lsof -i:50051 | sed "2q;d" | awk '{print $2}')
if [[ "$PID" != "" ]]; then
  sudo kill $PID
fi