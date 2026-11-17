# maven-args.sh: sourced by setup-java-maven's scripts. One rule for what an
# mvn command line is, for the cache key (maven-cache-key.sh), the step that
# declared it (restored.sh) and the mvn that runs it (bin/mvn).
#
# The flags that change only Maven's output or its update checks, not the
# plugins and artifacts it resolves, are left out: -q, -B, -e, -X, -V, -ntp,
# -U, -nsu, -o and their long forms. What is left is the goals and phases,
# the profiles, the -D properties, -f and the rest, in the order given: what
# decides the artifact set. So `-B -U clean package -q` and `clean package`
# are one command line, and `clean package dependency:tree` is another.
# Arguments are compared as words split on spaces, so a value with a space in
# it cannot be declared (no lane passes one).

# maven_args_keep ARG...: sets MAVEN_ARGS_KEPT to the arguments that count.
maven_args_keep() {
  local a
  MAVEN_ARGS_KEPT=()
  for a in "$@"; do
    case "$a" in
      -q | --quiet | -B | --batch-mode | -e | --errors | -X | --debug | -V | --show-version | \
        -ntp | --no-transfer-progress | -U | --update-snapshots | -nsu | --no-snapshot-updates | \
        -o | --offline) ;;
      *) MAVEN_ARGS_KEPT+=("$a") ;;
    esac
  done
}

# maven_args_normalize ARG...: prints the arguments that count, space-separated.
maven_args_normalize() {
  local IFS=' '
  maven_args_keep "$@"
  printf '%s' "${MAVEN_ARGS_KEPT[*]}"
}

# maven_args_shown ARG...: the same, with each -D property's value left out
# (-Dname=value prints as -Dname=...), for a log line about a command line
# nobody declared, which may carry a value that is not public.
maven_args_shown() {
  local IFS=' ' a
  local out=()
  maven_args_keep "$@"
  for a in "${MAVEN_ARGS_KEPT[@]}"; do
    case "$a" in
      -D*=*) out+=("${a%%=*}=...") ;;
      *) out+=("$a") ;;
    esac
  done
  printf '%s' "${out[*]}"
}

# maven_args_lines TEXT: each non-blank line of TEXT as a command line, split
# on spaces and normalized, one per line, sorted and without repeats.
maven_args_lines() {
  local line
  local words=()
  while IFS= read -r line; do
    read -r -a words <<<"$line" || true
    [ "${#words[@]}" -gt 0 ] || continue
    maven_args_normalize "${words[@]}"
    printf '\n'
  done <<<"$1" | awk 'NF' | LC_ALL=C sort -u
}
