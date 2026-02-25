#!/bin/bash
set -e

# Start PetClinic with Keploy recording
echo "🚀 Starting PetClinic with Keploy recording..."

# Create test directory
mkdir -p keploy-tests

# Get the PetClinic JAR path
PETCLINIC_JAR=$(ls ../petclinic/target/spring-petclinic-*.jar | head -1)

if [ -z "$PETCLINIC_JAR" ]; then
  echo "❌ ERROR: PetClinic JAR not found"
  exit 1
fi

echo "📦 Using JAR: $PETCLINIC_JAR"

# Start Keploy wrapping PetClinic application
sudo -E env PATH=$PATH ./keploy record \
  -c "java -jar $PETCLINIC_JAR --spring.datasource.url=jdbc:mysql://localhost:3306/petclinic --spring.datasource.username=root --spring.datasource.password=petclinic" \
  --path=./keploy-tests \
  --config-path=/tmp/keploy-no-config &

echo $! > keploy.pid
echo "📝 Keploy PID: $(cat keploy.pid)"

# Wait for PetClinic to be ready
echo "⏳ Waiting for PetClinic to be ready..."
for i in {1..30}; do
  if curl -f http://localhost:8080/actuator/health 2>/dev/null; then
    echo "✅ PetClinic with Keploy is ready"
    exit 0
  fi
  echo "Waiting for PetClinic with Keploy... ($i/30)"
  sleep 2
done

echo "❌ ERROR: PetClinic failed to start within 60 seconds"
exit 1
