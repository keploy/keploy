#!/bin/bash
set -e

# Start PetClinic with Keploy recording
echo "🚀 Starting PetClinic with Keploy recording..."

# Create test directory
mkdir -p keploy-tests

# Get the PetClinic JAR path
shopt -s nullglob
jars=(../petclinic/target/spring-petclinic-*.jar)
if [ ${#jars[@]} -eq 0 ]; then
  echo "❌ ERROR: PetClinic JAR not found"
  exit 1
fi
PETCLINIC_JAR="${jars[0]}"

echo "📦 Using JAR: $PETCLINIC_JAR"

# Start Keploy wrapping PetClinic application
sudo -E env PATH=$PATH ./keploy record \
  -c "java -Dspring.profiles.active=mysql -jar $PETCLINIC_JAR --spring.datasource.url=jdbc:mysql://localhost:3306/petclinic --spring.datasource.username=root --spring.datasource.password=petclinic" \
  --path=./keploy-tests \
  --config-path=/tmp/keploy-no-config &

echo $! > keploy.pid
echo "📝 Keploy PID: $(cat keploy.pid)"

# Wait for PetClinic to be ready
echo "⏳ Waiting for PetClinic to be ready..."
for i in {1..30}; do
  if curl -f http://localhost:8080/actuator/health 2>/dev/null; then
    # Verify Keploy process is still alive
    KEPLOY_PID=$(cat keploy.pid)
    if ! ps -p $KEPLOY_PID > /dev/null; then
      echo "❌ ERROR: PetClinic is healthy but Keploy process (PID: $KEPLOY_PID) has died!"
      exit 1
    fi
    echo "✅ PetClinic with Keploy is ready"
    exit 0
  fi
  echo "Waiting for PetClinic with Keploy... ($i/30)"
  # Also check if Keploy died during the wait
  if ! ps -p $(cat keploy.pid) > /dev/null; then
    echo "❌ ERROR: Keploy process died during startup"
    exit 1
  fi
  sleep 2
done

echo "❌ ERROR: PetClinic failed to start within 60 seconds"
exit 1
