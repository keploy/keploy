send_request(){
    sleep 10
    app_started=false
    while [ "$app_started" = false ]; do
        if curl --request POST \
          --url http://localhost:8080/url \
          --header 'content-type: application/json' \
          --data '{
          "url": "https://facebook.com"
        }'; then
            app_started=true
        fi
        sleep 3 # wait for 3 seconds before checking again.
    done
    echo "App started"      

    # Start making curl calls to record the test cases and mocks.
    curl --request POST \
      --url http://localhost:8080/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://google.com"
    }'

    curl --request POST \
      --url http://localhost:8080/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://facebook.com"
    }'

    curl -X GET http://localhost:8080/CJBKJd92

    # Wait for 10 seconds for keploy to record the tcs and mocks.
    sleep 10
    
    # Find and kill the keploy record process
    pid=$(pgrep -f 'keployv2 record')
    
    if [ -z "$pid" ]; then
        echo "Keploy record process not found. Skipping kill."
    else
        echo "$pid Keploy record PID"
        # Attempt to kill the process with a timeout
        sudo kill $pid
        
        # Check if process is still running after the kill command
        sleep 5
        if kill -0 $pid 2>/dev/null; then
            echo "Keploy record process did not stop. Forcing kill..."
            sudo kill -9 $pid
        else
            echo "Keploy record process stopped successfully."
        fi
    fi
}
