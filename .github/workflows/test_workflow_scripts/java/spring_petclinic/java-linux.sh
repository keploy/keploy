#!/usr/bin/env bash
# Safe, chatty CI script for Java + Postgres + Keploy with auto API-prefix detection

set -Eeuo pipefail

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

die() {
  rc=$?
  echo "::error::Pipeline failed (exit=$rc). Dumping context…"
  echo "== docker ps =="; docker ps || true
  echo "== postgres logs (last 200 lines) =="; docker logs --tail 200 mypostgres || true
  echo "== workspace tree (depth 3) =="; find . -maxdepth 3 -type d -print | sort || true
  echo "== keploy tree (depth 4 - first 20 lines) =="; find ./keploy -maxdepth 4 -type f -print 2>/dev/null | sort | head -n 20 || true; echo "... (truncated)"
  echo "== *.txt logs (last 100 lines) =="; for f in ./*.txt; do [[ -f "$f" ]] && { echo "--- $f ---"; tail -n 100 "$f"; }; done
  [[ -f test_logs.txt ]] && { echo "== test_logs.txt (last 100 lines) =="; tail -n 100 test_logs.txt; }
  exit "$rc"
}
trap die ERR

http_code() {
  # prints HTTP status code or 000 on error
  curl -s -o /dev/null -w "%{http_code}" "$1" 2>/dev/null || echo 000
}

wait_for_postgres() {
  section "Wait for Postgres readiness"
  for i in {1..120}; do
    if docker exec mypostgres pg_isready -U petclinic -d petclinic >/dev/null 2>&1; then
      echo "Postgres is ready."
      endsec; return 0
    fi
    # Fallback probe
    docker exec mypostgres psql -U petclinic -d petclinic -c "SELECT 1" >/dev/null 2>&1 && { echo "Postgres responded."; endsec; return 0; }
    sleep 1
  done
  echo "::error::Postgres did not become ready in time"
  endsec; return 1
}

wait_for_http_port() {
  # waits for *any* HTTP response from root or actuator (not strict 200)
  local base="http://localhost:9966"
  section "Wait for app HTTP port"
  for i in {1..180}; do
    if curl -sS "${base}/" -o /dev/null || curl -sS "${base}/actuator/health" -o /dev/null; then
      echo "HTTP port responded."
      endsec; return 0
    fi
    sleep 1
  done
  echo "::error::App did not open HTTP port on 9966"
  endsec; return 1
}

detect_api_prefix() {
  # returns either /petclinic/api or /api (echo to stdout), otherwise empty
  local base="http://localhost:9966"
  local candidates=( "/petclinic/api" "/api" )
  for p in "${candidates[@]}"; do
    local code
    code=$(http_code "${base}${p}/pettypes")
    if [[ "$code" =~ ^(200|201|202|204)$ ]]; then
      echo "$p"; return 0
    fi
  done
  # If no 2xx, still check which gives *any* non-404 (e.g., 401/403 if security toggled)
  for p in "${candidates[@]}"; do
    local code
    code=$(http_code "${base}${p}/pettypes")
    if [[ "$code" != "404" && "$code" != "000" ]]; then
      echo "$p"; return 0
    fi
  done
  # Fallback to actuator presence: assume /api if actuator exists
  if [[ "$(http_code "${base}/actuator/health")" == "200" ]]; then
    echo "/api"; return 0
  fi
  echo ""
  return 1
}

# --- USER PROVIDED HELPERS START ---

# Configuration
TOTAL_TRANSACTIONS=1000
REQUESTS_PER_CHAIN=12  # owner + get_owner + get_owner_ln + list_owners + pet + visit + list_visits + vet + get_vet + list_vets + list_pettypes + list_specialties
CHAINS_NEEDED=$((TOTAL_TRANSACTIONS / REQUESTS_PER_CHAIN))

# Counters - using temp files to persist across subshells
COUNTER_FILE=$(mktemp)
echo "0 0 0" > "$COUNTER_FILE"  # success failure total

increment_success() {
    local counts
    read -r success failure total < "$COUNTER_FILE"
    echo "$((success + 1)) $failure $((total + 1))" > "$COUNTER_FILE"
}

increment_failure() {
    local counts
    read -r success failure total < "$COUNTER_FILE"
    echo "$success $((failure + 1)) $((total + 1))" > "$COUNTER_FILE"
}

get_counts() {
    cat "$COUNTER_FILE"
}

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

# Cleanup function - deletes all data in correct order to respect FK constraints
cleanup_database() {
    log_info "Cleaning up existing data (respecting foreign key constraints)..."
    
    # Get all visits and delete them
    log_info "  Deleting visits..."
    local visits=$(curl -s "${BASE_URL}/visits" 2>/dev/null || echo "[]")
    if [ "$visits" != "[]" ] && [ -n "$visits" ]; then
        echo "$visits" | jq -r '.[].id' 2>/dev/null | while read -r id; do
            if [ -n "$id" ] && [ "$id" != "null" ]; then
                curl -s -X DELETE "${BASE_URL}/visits/${id}" > /dev/null 2>&1 || true
            fi
        done
    fi
    
    # Get all pets and delete them
    log_info "  Deleting pets..."
    local owners=$(curl -s "${BASE_URL}/owners" 2>/dev/null || echo "[]")
    if [ "$owners" != "[]" ] && [ -n "$owners" ]; then
        echo "$owners" | jq -r '.[].id' 2>/dev/null | while read -r owner_id; do
            if [ -n "$owner_id" ] && [ "$owner_id" != "null" ]; then
                local pets=$(curl -s "${BASE_URL}/owners/${owner_id}" 2>/dev/null | jq -r '.pets[].id' 2>/dev/null || echo "")
                for pet_id in $pets; do
                    if [ -n "$pet_id" ] && [ "$pet_id" != "null" ]; then
                        curl -s -X DELETE "${BASE_URL}/pets/${pet_id}" > /dev/null 2>&1 || true
                    fi
                done
            fi
        done
    fi
    
    # Delete all owners
    log_info "  Deleting owners..."
    if [ "$owners" != "[]" ] && [ -n "$owners" ]; then
        echo "$owners" | jq -r '.[].id' 2>/dev/null | while read -r id; do
            if [ -n "$id" ] && [ "$id" != "null" ]; then
                curl -s -X DELETE "${BASE_URL}/owners/${id}" > /dev/null 2>&1 || true
            fi
        done
    fi
    
    # Delete all vets
    log_info "  Deleting vets..."
    local vets=$(curl -s "${BASE_URL}/vets" 2>/dev/null || echo "[]")
    if [ "$vets" != "[]" ] && [ -n "$vets" ]; then
        echo "$vets" | jq -r '.[].id' 2>/dev/null | while read -r id; do
            if [ -n "$id" ] && [ "$id" != "null" ]; then
                curl -s -X DELETE "${BASE_URL}/vets/${id}" > /dev/null 2>&1 || true
            fi
        done
    fi
    
    # Delete all specialties
    log_info "  Deleting specialties..."
    local specialties=$(curl -s "${BASE_URL}/specialties" 2>/dev/null || echo "[]")
    if [ "$specialties" != "[]" ] && [ -n "$specialties" ]; then
        echo "$specialties" | jq -r '.[].id' 2>/dev/null | while read -r id; do
            if [ -n "$id" ] && [ "$id" != "null" ]; then
                curl -s -X DELETE "${BASE_URL}/specialties/${id}" > /dev/null 2>&1 || true
            fi
        done
    fi
    
    # Delete all pet types
    log_info "  Deleting pet types..."
    local pettypes=$(curl -s "${BASE_URL}/pettypes" 2>/dev/null || echo "[]")
    if [ "$pettypes" != "[]" ] && [ -n "$pettypes" ]; then
        echo "$pettypes" | jq -r '.[].id' 2>/dev/null | while read -r id; do
            if [ -n "$id" ] && [ "$id" != "null" ]; then
                curl -s -X DELETE "${BASE_URL}/pettypes/${id}" > /dev/null 2>&1 || true
            fi
        done
    fi
    
    log_success "Database cleanup complete!"
}

# API call function with response validation
api_call() {
    local method=$1
    local endpoint=$2
    local data=$3
    local expected_code=$4
    
    local response
    local http_code
    
    if [ "$method" = "POST" ]; then
        response=$(curl -s -w "\n%{http_code}" -X POST \
            -H "Content-Type: application/json" \
            -H "Accept: application/json" \
            -d "$data" \
            "${BASE_URL}${endpoint}" 2>/dev/null)
    elif [ "$method" = "GET" ]; then
        response=$(curl -s -w "\n%{http_code}" -X GET \
            -H "Accept: application/json" \
            "${BASE_URL}${endpoint}" 2>/dev/null)
    fi
    
    http_code=$(echo "$response" | tail -n1)
    local body=$(echo "$response" | sed '$d')
    
    if [ "$http_code" = "$expected_code" ]; then
        increment_success
        echo "$body"
        return 0
    else
        increment_failure
        echo -e "${RED}[FAILED]${NC} ${method} ${endpoint} - Expected: ${expected_code}, Got: ${http_code}" >&2
        echo -e "${RED}[BODY]${NC} ${body}" >&2
        return 1
    fi
}

# Create a pet type
create_pettype() {
    local name=$1
    local data="{\"id\":null,\"name\":\"${name}\"}"
    local response
    
    response=$(api_call "POST" "/pettypes" "$data" "201")
    if [ $? -eq 0 ]; then
        echo "$response" | jq -r '.id' 2>/dev/null
    else
        echo ""
    fi
}

# Create an owner
create_owner() {
    local firstName=$1
    local lastName=$2
    local address=$3
    local city=$4
    local telephone=$5
    local data="{\"id\":null,\"firstName\":\"${firstName}\",\"lastName\":\"${lastName}\",\"address\":\"${address}\",\"city\":\"${city}\",\"telephone\":\"${telephone}\"}"
    local response
    
    response=$(api_call "POST" "/owners" "$data" "201")
    if [ $? -eq 0 ]; then
        echo "$response" | jq -r '.id' 2>/dev/null
    else
        echo ""
    fi
}

# Create a pet for an owner
create_pet() {
    local name=$1
    local birthDate=$2
    local typeId=$3
    local typeName=$4
    local ownerId=$5
    local data="{\"id\":null,\"name\":\"${name}\",\"birthDate\":\"${birthDate}\",\"type\":{\"id\":${typeId},\"name\":\"${typeName}\"},\"owner\":{\"id\":${ownerId}}}"
    local response
    
    response=$(api_call "POST" "/owners/${ownerId}/pets" "$data" "201")
    if [ $? -eq 0 ]; then
        echo "$response" | jq -r '.id' 2>/dev/null
    else
        echo ""
    fi
}

# Create a visit for a pet
create_visit() {
    local petId=$1
    local visitDate=$2
    local description=$3
    local data="{\"id\":null,\"date\":\"${visitDate}\",\"description\":\"${description}\",\"pet\":{\"id\":${petId}}}"
    local response
    
    response=$(api_call "POST" "/visits" "$data" "201")
    if [ $? -eq 0 ]; then
        echo "$response" | jq -r '.id' 2>/dev/null
    else
        echo ""
    fi
}

# Create a specialty
create_specialty() {
    local name=$1
    local data="{\"id\":null,\"name\":\"${name}\"}"
    local response
    
    response=$(api_call "POST" "/specialties" "$data" "201")
    if [ $? -eq 0 ]; then
        echo "$response" | jq -r '.id' 2>/dev/null
    else
        echo ""
    fi
}

# Create a vet
create_vet() {
    local firstName=$1
    local lastName=$2
    local specialtyId=$3
    local specialtyName=$4
    local data
    
    if [ -n "$specialtyId" ] && [ "$specialtyId" != "" ]; then
        data="{\"id\":null,\"firstName\":\"${firstName}\",\"lastName\":\"${lastName}\",\"specialties\":[{\"id\":${specialtyId},\"name\":\"${specialtyName}\"}]}"
    else
        data="{\"id\":null,\"firstName\":\"${firstName}\",\"lastName\":\"${lastName}\",\"specialties\":[]}"
    fi
    local response
    
    response=$(api_call "POST" "/vets" "$data" "201")
    if [ $? -eq 0 ]; then
        echo "$response" | jq -r '.id' 2>/dev/null
    else
        echo ""
    fi
}

# Get owner by ID
get_owner() {
    local id=$1
    if [ -n "$id" ] && [ "$id" != "null" ]; then
        api_call "GET" "/owners/${id}" "" "200" > /dev/null
    fi
}

# Get owners by last name
get_owners_by_lastname() {
    local lastname=$1
    if [ -n "$lastname" ]; then
        api_call "GET" "/owners?lastName=${lastname}" "" "200" > /dev/null
    fi
}

# Get vet by ID
get_vet() {
    local id=$1
    if [ -n "$id" ] && [ "$id" != "null" ]; then
        api_call "GET" "/vets/${id}" "" "200" > /dev/null
    fi
}

# List all owners
list_owners() {
    api_call "GET" "/owners" "" "200" > /dev/null
}

# List all vets
list_vets() {
    api_call "GET" "/vets" "" "200" > /dev/null
}

# List all visits
list_visits() {
    api_call "GET" "/visits" "" "200" > /dev/null
}

# List all pet types
list_pettypes() {
    api_call "GET" "/pettypes" "" "200" > /dev/null
}

# List all specialties
list_specialties() {
    api_call "GET" "/specialties" "" "200" > /dev/null
}

# Generate random data helpers
FIRST_NAMES=("James" "Mary" "John" "Patricia" "Robert" "Jennifer" "Michael" "Linda" "William" "Elizabeth" "David" "Barbara" "Richard" "Susan" "Joseph" "Jessica" "Thomas" "Sarah" "Charles" "Karen")
LAST_NAMES=("Smith" "Johnson" "Williams" "Brown" "Jones" "Garcia" "Miller" "Davis" "Rodriguez" "Martinez" "Hernandez" "Lopez" "Gonzalez" "Wilson" "Anderson" "Thomas" "Taylor" "Moore" "Jackson" "Martin")
PET_NAMES=("Max" "Bella" "Charlie" "Luna" "Cooper" "Daisy" "Buddy" "Sadie" "Rocky" "Molly" "Bear" "Bailey" "Duke" "Maggie" "Tucker" "Sophie" "Jack" "Chloe" "Leo" "Zoe")
PET_TYPES=("Cat" "Dog" "Bird" "Hamster" "Snake" "Lizard" "Rabbit" "Guinea Pig" "Fish" "Turtle")
SPECIALTIES=("Radiology" "Surgery" "Dentistry" "Cardiology" "Dermatology" "Oncology" "Neurology" "Ophthalmology" "Orthopedics" "Internal Medicine")
CITIES=("New York" "Los Angeles" "Chicago" "Houston" "Phoenix" "Philadelphia" "San Antonio" "San Diego" "Dallas" "San Jose")

get_random_element() {
    local -n arr=$1
    local len=${#arr[@]}
    echo "${arr[$((RANDOM % len))]}"
}

# Main execution logic for transactions
run_transactions() {
    local start_time=$(date +%s)
    
    echo "=============================================="
    echo "  Spring PetClinic E2E Test Script"
    echo "  Target: ${TOTAL_TRANSACTIONS} transactions"
    echo "  Chains: ${CHAINS_NEEDED} (${REQUESTS_PER_CHAIN} requests each)"
    echo "=============================================="
    echo ""
    
    # Cleanup existing data
    cleanup_database
    
    log_info "Starting transaction generation..."
    echo ""
    
    # Pre-create some pet types and specialties to reuse
    log_info "Creating base pet types..."
    declare -a PETTYPE_IDS
    declare -a PETTYPE_NAMES
    for i in {0..9}; do
        local type_name="${PET_TYPES[$i]}"
        local type_id=$(create_pettype "$type_name")
        if [ -n "$type_id" ] && [ "$type_id" != "null" ]; then
            PETTYPE_IDS+=("$type_id")
            PETTYPE_NAMES+=("$type_name")
        fi
    done
    log_success "Created ${#PETTYPE_IDS[@]} pet types"
    
    log_info "Creating base specialties..."
    declare -a SPECIALTY_IDS
    declare -a SPECIALTY_NAMES
    for i in {0..9}; do
        local spec_name="${SPECIALTIES[$i]}"
        local spec_id=$(create_specialty "$spec_name")
        if [ -n "$spec_id" ] && [ "$spec_id" != "null" ]; then
            SPECIALTY_IDS+=("$spec_id")
            SPECIALTY_NAMES+=("$spec_name")
        fi
    done
    log_success "Created ${#SPECIALTY_IDS[@]} specialties"
    
    echo ""
    log_info "Creating ${CHAINS_NEEDED} transaction chains..."
    
    # Create transaction chains
    for i in $(seq 1 $CHAINS_NEEDED); do
        # Progress indicator
        if [ $((i % 20)) -eq 0 ]; then
            local percent=$((i * 100 / CHAINS_NEEDED))
            read -r SUCCESS_COUNT FAILURE_COUNT TOTAL_REQUESTS < "$COUNTER_FILE"
            echo -e "${BLUE}[PROGRESS]${NC} Chain ${i}/${CHAINS_NEEDED} (${percent}%) - Requests: ${TOTAL_REQUESTS}, Success: ${SUCCESS_COUNT}, Failed: ${FAILURE_COUNT}"
        fi
        
        # Random data for this chain
        local firstName=$(get_random_element FIRST_NAMES)
        local lastName=$(get_random_element LAST_NAMES)
        local city=$(get_random_element CITIES)
        local petName=$(get_random_element PET_NAMES)
        local address="${i} Main Street"
        local telephone="555$(printf '%07d' $i)"
        local birthDate="2020-$(printf '%02d' $((RANDOM % 12 + 1)))-$(printf '%02d' $((RANDOM % 28 + 1)))"
        local visitDate="2025-$(printf '%02d' $((RANDOM % 12 + 1)))-$(printf '%02d' $((RANDOM % 28 + 1)))"
        
        # Get random existing IDs and names
        local typeIdx=$((RANDOM % ${#PETTYPE_IDS[@]}))
        local typeId=${PETTYPE_IDS[$typeIdx]}
        local typeName=${PETTYPE_NAMES[$typeIdx]}
        local specIdx=$((RANDOM % ${#SPECIALTY_IDS[@]}))
        local specId=${SPECIALTY_IDS[$specIdx]}
        local specName=${SPECIALTY_NAMES[$specIdx]}
        
        # Create Owner - use letters only for names (API validates ^[a-zA-Z]*$)
        local uniqueSuffix=$(printf '%03d' $i | tr '0-9' 'a-j')
        local ownerId=$(create_owner "${firstName}" "${lastName}${uniqueSuffix}" "$address" "$city" "$telephone")
        
        if [ -n "$ownerId" ] && [ "$ownerId" != "null" ]; then
            # Get Owner by ID (triggers join)
            get_owner "$ownerId"
            
            # Get Owner by LastName (triggers join)
            get_owners_by_lastname "${lastName}${uniqueSuffix}"
            
            # List Owners
            list_owners

            # Create Pet for Owner - use letters only for pet names too
            local petId=$(create_pet "${petName}${uniqueSuffix}" "$birthDate" "$typeId" "$typeName" "$ownerId")
            
            if [ -n "$petId" ] && [ "$petId" != "null" ]; then
                # Create Visit for Pet
                create_visit "$petId" "$visitDate" "Regular checkup" > /dev/null
                
                # List Visits
                list_visits
            fi
        fi
        
        # Create Vet with specialty
        local vetId=$(create_vet "${firstName}" "${lastName}${uniqueSuffix}" "$specId" "$specName")
        
        if [ -n "$vetId" ] && [ "$vetId" != "null" ]; then
             get_vet "$vetId"
        fi
        
        # List Vets
        list_vets
        
        # List Pet Types
        list_pettypes
        
        # List Specialties
        list_specialties
    done
    
    local end_time=$(date +%s)
    local duration=$((end_time - start_time))
    
    echo ""
    echo "=============================================="
    echo "  E2E Test Complete!"
    echo "=============================================="
    echo ""
    log_info "Summary:"
    read -r SUCCESS_COUNT FAILURE_COUNT TOTAL_REQUESTS < "$COUNTER_FILE"
    echo "  Total Requests:    ${TOTAL_REQUESTS}"
    echo "  Successful:        ${SUCCESS_COUNT}"
    echo "  Failed:            ${FAILURE_COUNT}"
    echo "  Duration:          ${duration} seconds"
    echo "  Requests/sec:      $(echo "scale=2; ${TOTAL_REQUESTS} / ${duration}" | bc 2>/dev/null || echo "N/A")"
    echo ""
    
    # Verify counts
    log_info "Verifying final entity counts..."
    local owner_count=$(curl -s "${BASE_URL}/owners" | jq 'length' 2>/dev/null || echo "N/A")
    local vet_count=$(curl -s "${BASE_URL}/vets" | jq 'length' 2>/dev/null || echo "N/A")
    local pettype_count=$(curl -s "${BASE_URL}/pettypes" | jq 'length' 2>/dev/null || echo "N/A")
    local specialty_count=$(curl -s "${BASE_URL}/specialties" | jq 'length' 2>/dev/null || echo "N/A")
    
    echo "  Owners:       ${owner_count}"
    echo "  Vets:         ${vet_count}"
    echo "  Pet Types:    ${pettype_count}"
    echo "  Specialties:  ${specialty_count}"
    echo ""
    
    # Cleanup temp file
    rm -f "$COUNTER_FILE"
    
    if [ $FAILURE_COUNT -eq 0 ]; then
        log_success "All requests completed successfully!"
    else
        log_warning "${FAILURE_COUNT} requests failed"
        # We don't exit here to allow Keploy to stop gracefully in send_request
    fi
}

send_request() {
  local kp_pid="$1"
  local base="http://localhost:9966"

  wait_for_http_port

  # Try to detect API prefix dynamically
  local API_PREFIX
  API_PREFIX=$(detect_api_prefix || true)
  
  if [[ -z "${API_PREFIX}" ]]; then
     echo "::warning::Could not auto-detect API prefix. Defaulting to /petclinic/api"
     API_PREFIX="/petclinic/api"
  fi
  
  echo "Detected API prefix: ${API_PREFIX}"
  export BASE_URL="${base}${API_PREFIX}"
  
  # Initialize counter file for this run
  echo "0 0 0" > "$COUNTER_FILE"
  
  # Run the user's transaction logic
  run_transactions

  # Let keploy persist, then stop it
  sleep 10
  echo "$kp_pid Keploy PID"
  echo "Killing keploy"
  sudo kill "$kp_pid" 2>/dev/null || true
}

# ----- main -----

source ./../../../.github/workflows/test_workflow_scripts/test-iid.sh

section "Git branch"
git fetch origin
git checkout petclinic-script
endsec

section "Start Postgres"
docker run -d --name mypostgres -e POSTGRES_USER=petclinic -e POSTGRES_PASSWORD=petclinic \
  -e POSTGRES_DB=petclinic -p 5432:5432 postgres:latest
wait_for_postgres
# seed DB
docker cp ./src/main/resources/db/postgresql/initDB.sql mypostgres:/initDB.sql
docker exec mypostgres psql -U petclinic -d petclinic -f /initDB.sql
endsec

section "Java setup"
source ./../../../.github/workflows/test_workflow_scripts/update-java.sh
endsec

# Clean once (keep artifacts across iterations)
sudo rm -rf keploy/

for i in 1; do
  section "Record iteration $i"

  # Build app (captured to log)
  mvn clean install -Dmaven.test.skip=true | tee -a mvn_build.log

  app_name="javaApp_${i}"

  # Start keploy in background, capture PID
  "$RECORD_BIN" record \
    -c 'java -jar target/spring-petclinic-rest-3.0.2.jar' \
    > "${app_name}.txt" 2>&1 &
  KEPLOY_PID=$!

  # Drive traffic and stop keploy
  send_request "$KEPLOY_PID"

  # Wait for keploy exit and capture code
  set +e
  wait "$KEPLOY_PID"
  rc=$?
  set -e
  echo "Record exit code: $rc"
  [[ $rc -ne 0 ]] && echo "::warning::Keploy record exited non-zero (iteration $i)"

  # Quick sanity: ensure something was written
  echo "== keploy artifacts after record =="
  find ./keploy -maxdepth 3 -type f | wc -l || true

  # Surface issues from record logs
  if grep -q "WARNING: DATA RACE" "${app_name}.txt"; then
    echo "::error::Data race detected in ${app_name}.txt"
    cat "${app_name}.txt"
    exit 1
  fi
  if grep -q "ERROR" "${app_name}.txt"; then
    echo "::warning::Errors found in ${app_name}.txt"
    cat "${app_name}.txt"
  fi

  endsec
  echo "Recorded test case and mocks for iteration ${i}"
done

sleep 5

section "Shutdown Postgres before test mode"
# Stop Postgres container - Keploy should use mocks for database interactions
docker stop mypostgres || true
docker rm mypostgres || true
echo "Postgres stopped - Keploy should now use mocks for database interactions"
endsec

section "Replay"
set +e
"$REPLAY_BIN" test \
  -c 'java -jar target/spring-petclinic-rest-3.0.2.jar' \
  --delay 20 --debug \
  2>&1 | tee test_logs.txt
REPLAY_RC=$?
set -e
echo "Replay exit code: $REPLAY_RC"
endsec

# ✅ Extract and validate coverage percentage from log
coverage_line=$(grep -Eo "Total Coverage Percentage:[[:space:]]+[0-9]+(\.[0-9]+)?%" "test_logs.txt" | tail -n1 || true)

if [[ -z "$coverage_line" ]]; then
  echo "::error::No coverage percentage found in test_logs.txt"
  return 1
fi

coverage_percent=$(echo "$coverage_line" | grep -Eo "[0-9]+(\.[0-9]+)?" || echo "0")
echo "📊 Extracted coverage: ${coverage_percent}%"

# Fail if coverage ≤ 0%
if (( $(echo "$coverage_percent <= 0" | bc -l) )); then
  echo "::error::Coverage below threshold (0%). Found: ${coverage_percent}%"
  exit 1
else
  echo "✅ Coverage meets threshold (> 0%)"
fi

section "Check reports"
RUN_DIR=$(ls -1dt ./keploy/reports/test-run-* 2>/dev/null | head -n1 || true)
if [[ -z "${RUN_DIR:-}" ]]; then
  echo "::error::No test-run directory found under ./keploy/reports"
  [[ $REPLAY_RC -ne 0 ]] && exit "$REPLAY_RC" || exit 1
fi
echo "Using reports from: $RUN_DIR"

all_passed=true
for rpt in "$RUN_DIR"/test-set-*-report.yaml; do
  [[ -f "$rpt" ]] || continue
  status=$(awk '/^status:/{print $2; exit}' "$rpt")
  echo "Test status for $(basename "$rpt"): ${status:-<missing>}"
  [[ "$status" == "PASSED" ]] || all_passed=false
done
endsec

if [[ "$all_passed" == "true" && $REPLAY_RC -eq 0 ]]; then
  echo "All tests passed"
  exit 0
fi

echo "::error::Some tests failed or replay exited non-zero"
exit 1