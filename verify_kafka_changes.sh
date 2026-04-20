#!/bin/bash

echo "=========================================="
echo "Verifying Kafka Mock Type Fix Changes"
echo "=========================================="
echo ""

echo "1. Checking if Keploy binary is updated..."
KEPLOY_VERSION=$(keploy --version 2>&1)
echo "   Keploy version: $KEPLOY_VERSION"
echo ""

echo "2. Checking coordination API classification in integrations..."
cd /home/yogesh/projects/integrations
echo "   Running coordination API tests..."
go test -v ./pkg/kafka/recorder/recorder_mock_type_test.go ./pkg/kafka/recorder/recorder.go 2>&1 | grep -E "(PASS|FAIL|TestIsCoordinationAPI|TestCoordinationAPIsMap)"
echo ""

echo "3. Verifying recorder changes..."
if grep -q "coordinationAPIs.*map\[string\]bool" pkg/kafka/recorder/recorder.go; then
    echo "   ✅ coordinationAPIs map found in recorder.go"
else
    echo "   ❌ coordinationAPIs map NOT found in recorder.go"
fi

if grep -q "isCoordinationAPI" pkg/kafka/recorder/recorder.go; then
    echo "   ✅ isCoordinationAPI function found in recorder.go"
else
    echo "   ❌ isCoordinationAPI function NOT found in recorder.go"
fi

if grep -q 'mockType := "mocks"' pkg/kafka/recorder/recorder.go; then
    echo "   ✅ Dynamic mock type logic found in recorder.go"
else
    echo "   ❌ Dynamic mock type logic NOT found in recorder.go"
fi
echo ""

echo "4. Verifying replayer changes..."
if grep -q "createErrorResponse" pkg/kafka/replayer/replayer.go; then
    echo "   ✅ createErrorResponse function found in replayer.go"
else
    echo "   ❌ createErrorResponse function NOT found in replayer.go"
fi

if grep -q "sendErrorResponse" pkg/kafka/replayer/replayer.go; then
    echo "   ✅ sendErrorResponse function found in replayer.go"
else
    echo "   ❌ sendErrorResponse function NOT found in replayer.go"
fi

if grep -q "No matching mock found - test mode requires all calls to be mocked" pkg/kafka/replayer/replayer.go; then
    echo "   ✅ Pass-through prevention logic found in replayer.go"
else
    echo "   ❌ Pass-through prevention logic NOT found in replayer.go"
fi
echo ""

echo "=========================================="
echo "Verification Complete!"
echo "=========================================="
echo ""
echo "Next steps:"
echo "1. Re-record mocks with: sudo keploy record -c 'docker compose up' --container-name='order_service' --path='./order_service'"
echo "2. Run tests with: sudo keploy test -c 'docker compose up' --container-name='order_service' --path='./order_service'"
echo "3. Check logs for 'configReused' messages for coordination APIs"
