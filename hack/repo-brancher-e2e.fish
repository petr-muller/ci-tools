#!/usr/bin/env fish
#
# E2E test harness for repo-brancher
#
# Maintains 20 test repositories in muller-testing-org with varying states
# of release-branch freshness. Compares a "vanilla" binary against a
# "modified" one for correctness and performance.
#
# The tool being tested fast-forwards release branches (release-4.18) to
# match the current development branch (main). Test repos are divided into
# four groups to exercise different code paths:
#
#   No release branch  (5 repos): branch must be created from scratch
#   Slightly behind    (5 repos): 1-3 commits behind main (shallow push works)
#   Far behind         (5 repos): 30-200 commits behind (needs progressive deepening)
#   Already synced     (5 repos): at main HEAD (no-op)
#
# Prerequisites:
#   - gh CLI authenticated with push access to muller-testing-org
#   - GitHub token file readable by the binary (for git push via HTTPS)
#   - Both binaries built beforehand
#
# Usage:
#   ./repo-brancher-e2e.fish <vanilla-binary> <modified-binary>
#   ./repo-brancher-e2e.fish --setup               # create repos and populate with commits
#   ./repo-brancher-e2e.fish --reset-only           # just reset repos to initial state
#   ./repo-brancher-e2e.fish --single <binary>      # single run (no comparison)

# ── Configuration ────────────────────────────────────────────────────────────

set TARGET_ORG muller-testing-org
set TOKEN_PATH ~/.config/private-org-sync/token
set GH_USERNAME petr-muller
set CURRENT_RELEASE 4.17
set FUTURE_RELEASE 4.18
set FUTURE_BRANCH release-$FUTURE_RELEASE

# The tool also creates release-$CURRENT_RELEASE (it adds --current-release
# to the future releases set), so we must clean it up during reset too.
set CURRENT_BRANCH release-$CURRENT_RELEASE

# Repo specifications: "name:total_commits:behind"
#   total_commits = number of commits to create on main during setup
#   behind = how many commits behind main the release branch should be
#            0  = no release branch (will be created by the tool)
#            -1 = at HEAD (already synced, no-op)
#            N  = N commits behind main
set REPO_SPECS \
    "brancher-new-01:30:0" \
    "brancher-new-02:30:0" \
    "brancher-new-03:30:0" \
    "brancher-new-04:30:0" \
    "brancher-new-05:30:0" \
    "brancher-close-01:30:1" \
    "brancher-close-02:30:1" \
    "brancher-close-03:30:2" \
    "brancher-close-04:30:3" \
    "brancher-close-05:30:3" \
    "brancher-far-01:60:30" \
    "brancher-far-02:90:60" \
    "brancher-far-03:130:100" \
    "brancher-far-04:180:150" \
    "brancher-far-05:230:200" \
    "brancher-synced-01:30:-1" \
    "brancher-synced-02:30:-1" \
    "brancher-synced-03:30:-1" \
    "brancher-synced-04:30:-1" \
    "brancher-synced-05:30:-1"

# Extract repo names
set ALL_REPOS
for spec in $REPO_SPECS
    set -a ALL_REPOS (string split : -- $spec)[1]
end

# ── Helpers ──────────────────────────────────────────────────────────────────

function log
    set_color yellow
    echo ">>> $argv"
    set_color normal
end

function log_error
    set_color red
    echo "ERROR: $argv" >&2
    set_color normal
end

# URL-encode a ref name for GitHub API (handles slashes)
function encode_ref
    string replace -a / '%2F' -- $argv[1]
end

# ── Pre-flight checks ───────────────────────────────────────────────────────

function preflight
    if not command -q gh
        log_error "gh CLI not found"
        return 1
    end

    if not test -f $TOKEN_PATH
        log_error "Token file not found at $TOKEN_PATH"
        echo "  Create it with: mkdir -p (dirname $TOKEN_PATH) && echo YOUR_TOKEN > $TOKEN_PATH" >&2
        return 1
    end

    # Check destination repos exist (with retry)
    set -l missing 0
    for repo in $ALL_REPOS
        set -l found false
        for attempt in 1 2 3
            if gh api repos/$TARGET_ORG/$repo --silent 2>/dev/null
                set found true
                break
            end
            test $attempt -lt 3; and sleep 2
        end
        if not $found
            log_error "Repo $TARGET_ORG/$repo does not exist (run --setup first)"
            set missing (math $missing + 1)
        end
    end
    if test $missing -gt 0
        log_error "$missing repo(s) missing — run --setup to create them"
        return 1
    end

    log "Pre-flight checks passed"
end

# Pre-flight for setup mode: only check gh and token, not repo existence
function preflight_setup
    if not command -q gh
        log_error "gh CLI not found"
        return 1
    end

    if not test -f $TOKEN_PATH
        log_error "Token file not found at $TOKEN_PATH"
        echo "  Create it with: mkdir -p (dirname $TOKEN_PATH) && echo YOUR_TOKEN > $TOKEN_PATH" >&2
        return 1
    end

    log "Pre-flight checks passed (setup mode)"
end

# ── CI Operator config generation ────────────────────────────────────────────

# Generate minimal CI Operator configs that the repo-brancher will pick up.
# Each config promotes to ocp/$CURRENT_RELEASE from the main branch.
function generate_configs
    set -l config_dir $argv[1]
    for repo in $ALL_REPOS
        set -l repo_dir "$config_dir/$TARGET_ORG/$repo"
        mkdir -p "$repo_dir"
        set -l config_file "$repo_dir/$TARGET_ORG-$repo-main.yaml"
        begin
            echo "promotion:"
            echo "  to:"
            echo "  - name: \"$CURRENT_RELEASE\""
            echo "    namespace: ocp"
            echo "resources:"
            echo "  '*':"
            echo "    requests:"
            echo "      cpu: 100m"
            echo "      memory: 200Mi"
            echo "zz_generated_metadata:"
            echo "  branch: main"
            echo "  org: $TARGET_ORG"
            echo "  repo: $repo"
        end >$config_file
    end
    log "Generated CI Operator configs for "(count $ALL_REPOS)" repos in $config_dir"
end

# ── Setup: create repos and populate with commits ────────────────────────────

function setup_repo
    set -l repo $argv[1]
    set -l num_commits $argv[2]
    set -l behind $argv[3]
    set -l clone_dir $argv[4]

    log "  Setting up $TARGET_ORG/$repo ($num_commits commits, behind=$behind)"

    set -l repo_dir "$clone_dir/$repo"
    set -l token (string trim < $TOKEN_PATH)
    set -l remote_url "https://$GH_USERNAME:$token@github.com/$TARGET_ORG/$repo.git"

    # Create repo if it doesn't exist
    if not gh api repos/$TARGET_ORG/$repo --silent 2>/dev/null
        gh repo create $TARGET_ORG/$repo --public \
            --description "repo-brancher E2E test repo" 2>/dev/null
        or begin
            log_error "    Failed to create $TARGET_ORG/$repo"
            return 1
        end
    end

    # Initialize a fresh local repo
    mkdir -p $repo_dir
    git -C $repo_dir init --quiet
    git -C $repo_dir remote add origin $remote_url 2>/dev/null
    or git -C $repo_dir remote set-url origin $remote_url
    git -C $repo_dir config user.name "Petr Muller"
    git -C $repo_dir config user.email "afri@afri.cz"

    # Start on main
    git -C $repo_dir checkout -b main --quiet 2>/dev/null

    # Generate commits
    for i in (seq 1 $num_commits)
        echo "Commit $i - repo=$repo - "(random) >> $repo_dir/content.txt
        git -C $repo_dir add content.txt
        git -C $repo_dir commit -m "setup: commit $i/$num_commits" --quiet
    end

    # Force-push main
    if not git -C $repo_dir push --force origin main --quiet 2>&1
        log_error "    Failed to push main for $repo"
        return 1
    end

    # Set default branch to main
    gh api -X PATCH "repos/$TARGET_ORG/$repo" -f default_branch=main --silent 2>/dev/null

    # Handle release branch
    if test "$behind" = "0"
        # No release branch — delete if exists
        git -C $repo_dir push origin :refs/heads/$FUTURE_BRANCH --quiet 2>/dev/null
        log "    $num_commits commits, no $FUTURE_BRANCH"
    else if test "$behind" = "-1"
        # At HEAD (already synced)
        git -C $repo_dir push --force origin "main:refs/heads/$FUTURE_BRANCH" --quiet 2>&1
        log "    $num_commits commits, $FUTURE_BRANCH at HEAD (synced)"
    else
        # N commits behind
        set -l target_sha (git -C $repo_dir rev-parse "HEAD~$behind")
        git -C $repo_dir push --force origin "$target_sha:refs/heads/$FUTURE_BRANCH" --quiet 2>&1
        log "    $num_commits commits, $FUTURE_BRANCH $behind commits behind"
    end

    return 0
end

function setup_all
    set -l clone_dir (mktemp -d /tmp/repo-brancher-setup.XXXXXX)
    log "Setup working directory: $clone_dir"

    set -l failed 0
    for spec in $REPO_SPECS
        set -l fields (string split : -- $spec)
        set -l name $fields[1]
        set -l commits $fields[2]
        set -l behind $fields[3]
        if not setup_repo $name $commits $behind $clone_dir
            set failed (math $failed + 1)
        end
    end

    rm -rf $clone_dir

    if test $failed -gt 0
        log_error "Setup failed for $failed repo(s)"
        return 1
    end

    log "Setup complete: "(count $ALL_REPOS)" repos ready"
end

# ── Reset: restore repos to test-ready state via GitHub API ──────────────────

function get_main_sha
    set -l repo $argv[1]
    for attempt in 1 2 3
        set -l sha (gh api repos/$TARGET_ORG/$repo/commits/main -q '.sha' 2>/dev/null)
        if test -n "$sha"
            echo $sha
            return 0
        end
        test $attempt -lt 3; and sleep 2
    end
    log_error "    $repo: failed to get main SHA after 3 attempts"
    return 1
end

function get_sha_behind
    set -l repo $argv[1]
    set -l behind $argv[2]
    set -l target_index (math $behind + 1)

    set -l shas (gh api "repos/$TARGET_ORG/$repo/commits?sha=main&per_page=100" \
        --paginate -q '.[].sha' 2>/dev/null)

    if test (count $shas) -lt $target_index
        log_error "    $repo: not enough commits (need $target_index, have "(count $shas)")"
        return 1
    end

    echo $shas[$target_index]
end

function delete_branch
    set -l repo $argv[1]
    set -l branch $argv[2]
    set -l encoded (encode_ref $branch)
    gh api -X DELETE "repos/$TARGET_ORG/$repo/git/refs/heads/$encoded" --silent 2>/dev/null
    # Ignore errors — branch might not exist
    return 0
end

function force_update_branch
    set -l repo $argv[1]
    set -l branch $argv[2]
    set -l sha $argv[3]
    set -l encoded (encode_ref $branch)

    # Try update first (branch might already exist)
    if gh api -X PATCH "repos/$TARGET_ORG/$repo/git/refs/heads/$encoded" \
            -f sha=$sha -F force=true --silent 2>/dev/null
        return 0
    end

    # Branch doesn't exist — create it
    gh api -X POST "repos/$TARGET_ORG/$repo/git/refs" \
        -f ref="refs/heads/$branch" \
        -f sha=$sha --silent 2>/dev/null
end

function reset_all
    log "Resetting repos to initial state..."
    set -l failed 0

    for spec in $REPO_SPECS
        set -l fields (string split : -- $spec)
        set -l name $fields[1]
        set -l behind $fields[3]

        # Always delete release-$CURRENT_RELEASE (the tool creates it too)
        delete_branch $name $CURRENT_BRANCH

        if test "$behind" = "0"
            # No release branch — delete it
            delete_branch $name $FUTURE_BRANCH
            log "  $name: deleted $FUTURE_BRANCH"
        else if test "$behind" = "-1"
            # Set to main HEAD (already synced)
            set -l sha (get_main_sha $name)
            if test -z "$sha"
                set failed (math $failed + 1)
                continue
            end
            force_update_branch $name $FUTURE_BRANCH $sha
            log "  $name: $FUTURE_BRANCH -> main HEAD"
        else
            # Set to N commits behind main
            set -l sha (get_sha_behind $name $behind)
            if test -z "$sha"
                set failed (math $failed + 1)
                continue
            end
            force_update_branch $name $FUTURE_BRANCH $sha
            log "  $name: $FUTURE_BRANCH -> $behind behind main"
        end
    end

    if test $failed -gt 0
        log_error "Reset failed for $failed repo(s)"
        return 1
    end

    log "Reset complete"
end

# ── Snapshot: record branch SHAs for all repos ───────────────────────────────
# Format: "repo branch sha" per line, sorted, for easy diff comparison.

function snapshot_repo
    set -l repo $argv[1]
    set -l outfile $argv[2]
    for attempt in 1 2 3
        set -l lines (gh api repos/$TARGET_ORG/$repo/branches --paginate \
            -q '.[] | "\(.name) \(.commit.sha)"' 2>/dev/null | sort)
        if test (count $lines) -gt 0
            for line in $lines
                echo "$repo $line" >>$outfile
            end
            return 0
        end
        if test $attempt -lt 3
            log_error "  snapshot $TARGET_ORG/$repo failed (attempt $attempt), retrying..."
            sleep 2
        end
    end
    log_error "  snapshot $TARGET_ORG/$repo failed after 3 attempts"
    return 1
end

function snapshot_all
    set -l outfile $argv[1]
    true >$outfile
    set -l failed 0
    for repo in $ALL_REPOS
        if not snapshot_repo $repo $outfile
            set failed (math $failed + 1)
        end
    end
    if test $failed -gt 0
        log_error "Snapshot incomplete: $failed repo(s) failed"
        return 1
    end
end

# ── Run the repo-brancher binary ─────────────────────────────────────────────

function run_brancher
    set -l binary $argv[1]
    set -l label $argv[2]
    set -l config_dir $argv[3]
    set -l logfile $argv[4]

    log "Running repo-brancher [$label]: $binary"

    set -l start_ns (date +%s%N)

    $binary \
        --config-dir=$config_dir \
        --current-release=$CURRENT_RELEASE \
        --future-release=$FUTURE_RELEASE \
        --org=$TARGET_ORG \
        --username=$GH_USERNAME \
        --token-path=$TOKEN_PATH \
        --confirm \
        --log-level=debug \
        2>&1 | tee $logfile

    set -l rc $pipestatus[1]
    set -l end_ns (date +%s%N)
    set -l duration_ms (math "($end_ns - $start_ns) / 1000000")

    log "[$label] finished in {$duration_ms}ms (exit code $rc)"

    set -g __brancher_duration_ms $duration_ms
    set -g __brancher_exit_code $rc
    return $rc
end

# ── Verification ─────────────────────────────────────────────────────────────
# After a successful run, every repo's release branches should match main HEAD.

function verify_sync
    set -l snapshot_file $argv[1]
    set -l label $argv[2]
    set -l failures 0

    for repo in $ALL_REPOS
        # Get the main SHA from the snapshot
        set -l main_line (grep "^$repo main " $snapshot_file)
        if test -z "$main_line"
            set_color red
            echo "  FAIL: $repo — main branch not found in snapshot"
            set_color normal
            set failures (math $failures + 1)
            continue
        end
        set -l main_sha (string split " " -- $main_line)[3]

        # Check all release-* branches match main
        for release_line in (grep "^$repo release-" $snapshot_file)
            set -l parts (string split " " -- $release_line)
            set -l branch $parts[2]
            set -l sha $parts[3]
            if test "$sha" != "$main_sha"
                set_color red
                echo "  FAIL: $repo $branch=$sha != main=$main_sha"
                set_color normal
                set failures (math $failures + 1)
            end
        end

        # Check that both expected release branches exist
        for expected in $CURRENT_BRANCH $FUTURE_BRANCH
            if not grep -q "^$repo $expected " $snapshot_file
                set_color red
                echo "  FAIL: $repo — $expected does not exist"
                set_color normal
                set failures (math $failures + 1)
            end
        end
    end

    if test $failures -eq 0
        set_color green
        echo "PASS: [$label] all "(count $ALL_REPOS)" repos fully synced"
        set_color normal
        return 0
    else
        set_color red
        echo "FAIL: [$label] $failures issue(s) found"
        set_color normal
        return 1
    end
end

# ── Comparison ───────────────────────────────────────────────────────────────

function compare_snapshots
    set -l file_a $argv[1]
    set -l file_b $argv[2]
    set -l label_a $argv[3]
    set -l label_b $argv[4]

    if diff -q $file_a $file_b >/dev/null 2>&1
        set_color green
        echo "PASS: $label_a and $label_b produced identical destination state"
        set_color normal
        return 0
    else
        set_color red
        echo "DIFF: destination states differ between $label_a and $label_b:"
        set_color normal
        diff -u --label=$label_a --label=$label_b $file_a $file_b
        return 1
    end
end

# ── Performance reporting ────────────────────────────────────────────────────

function report_comparison
    set -l label $argv[1]
    set -l vanilla_t $argv[2]
    set -l vanilla_r $argv[3]
    set -l modified_t $argv[4]
    set -l modified_r $argv[5]

    echo "$label:"
    echo "  Vanilla:  {$vanilla_t}ms  (exit $vanilla_r)"
    echo "  Modified: {$modified_t}ms  (exit $modified_r)"
    if test "$vanilla_t" -gt 0
        set -l delta (math "$vanilla_t - $modified_t")
        set -l pct (math -s1 "100.0 * $delta / $vanilla_t")
        if test "$delta" -gt 0
            set_color green
            echo "  Modified is {$pct}% faster ({$delta}ms saved)"
        else if test "$delta" -lt 0
            set_color red
            set delta (math "- $delta")
            set pct (math -s1 "- $pct")
            echo "  Modified is {$pct}% slower ({$delta}ms added)"
        else
            echo "  Identical timing"
        end
        set_color normal
    end
end

# ── Main ─────────────────────────────────────────────────────────────────────

# Parse mode
set MODE compare
set BINARIES

switch "$argv[1]"
    case --setup
        set MODE setup
    case --reset-only
        set MODE reset
    case --single
        set MODE single
        if test (count $argv) -lt 2
            echo "Usage: $0 --single <binary>"
            exit 1
        end
        set BINARIES $argv[2]
    case --help -h
        echo "Usage:"
        echo "  $0 <vanilla-binary> <modified-binary>   Compare two builds"
        echo "  $0 --setup                              Create repos and populate with commits"
        echo "  $0 --reset-only                         Just reset repos to initial state"
        echo "  $0 --single <binary>                    Single run, verify correctness"
        echo ""
        echo "Configuration (edit the script to change):"
        echo "  TARGET_ORG       = $TARGET_ORG"
        echo "  TOKEN_PATH       = $TOKEN_PATH"
        echo "  GH_USERNAME      = $GH_USERNAME"
        echo "  CURRENT_RELEASE  = $CURRENT_RELEASE"
        echo "  FUTURE_RELEASE   = $FUTURE_RELEASE"
        echo "  FUTURE_BRANCH    = $FUTURE_BRANCH"
        echo "  Repos            = "(count $ALL_REPOS)" total"
        echo ""
        echo "Repo groups:"
        echo "  No release branch  (new):    brancher-new-{01..05}"
        echo "  Slightly behind    (close):  brancher-close-{01..05}  (1-3 commits)"
        echo "  Far behind         (far):    brancher-far-{01..05}    (30-200 commits)"
        echo "  Already synced     (synced): brancher-synced-{01..05}"
        exit 0
    case '*'
        set MODE compare
        if test (count $argv) -lt 2
            echo "Usage: $0 <vanilla-binary> <modified-binary>"
            echo "       $0 --help"
            exit 1
        end
        set BINARIES $argv[1] $argv[2]
end

# ── Mode: setup ──────────────────────────────────────────────────────────────
if test "$MODE" = setup
    preflight_setup; or exit 1
    setup_all
    exit $status
end

# Validate binaries
for bin in $BINARIES
    if not test -x $bin
        log_error "Binary not found or not executable: $bin"
        exit 1
    end
end

# Pre-flight
preflight; or exit 1

# Work directory
set WORK_DIR (mktemp -d /tmp/repo-brancher-e2e.XXXXXX)
set CONFIG_DIR $WORK_DIR/config
mkdir -p $CONFIG_DIR

log "Work directory: $WORK_DIR"

# Generate CI Operator configs
generate_configs $CONFIG_DIR

# ── Mode: reset-only ─────────────────────────────────────────────────────────
if test "$MODE" = reset
    reset_all
    log "Done. Repos have been reset."
    exit $status
end

# ── Mode: single ─────────────────────────────────────────────────────────────
if test "$MODE" = single
    log "=== Single run ==="
    reset_all; or exit 1

    run_brancher $BINARIES[1] single $CONFIG_DIR $WORK_DIR/single.log
    set -l duration $__brancher_duration_ms

    log "Snapshotting destination after sync..."
    snapshot_all $WORK_DIR/snapshot-single.txt
    verify_sync $WORK_DIR/snapshot-single.txt single

    echo ""
    log "Duration: {$duration}ms"
    log "Artifacts in $WORK_DIR"
    exit 0
end

# ── Mode: compare (default) ──────────────────────────────────────────────────
log "=== Phase 1: Vanilla run ==="
reset_all; or exit 1

log "Snapshotting destination after reset (pre-vanilla)..."
snapshot_all $WORK_DIR/snapshot-pre-vanilla.txt

run_brancher $BINARIES[1] vanilla $CONFIG_DIR $WORK_DIR/vanilla.log
set -l vanilla_ms $__brancher_duration_ms
set -l vanilla_rc $__brancher_exit_code

log "Snapshotting destination after vanilla run..."
snapshot_all $WORK_DIR/snapshot-vanilla.txt

log "=== Phase 2: Vanilla no-op run (already reconciled) ==="
run_brancher $BINARIES[1] vanilla-noop $CONFIG_DIR $WORK_DIR/vanilla-noop.log
set -l vanilla_noop_ms $__brancher_duration_ms
set -l vanilla_noop_rc $__brancher_exit_code

log "=== Phase 3: Modified run ==="
reset_all; or exit 1

log "Snapshotting destination after reset (pre-modified)..."
snapshot_all $WORK_DIR/snapshot-pre-modified.txt

# Validate both runs start from the same state
if not diff -q $WORK_DIR/snapshot-pre-vanilla.txt $WORK_DIR/snapshot-pre-modified.txt >/dev/null 2>&1
    set_color red
    echo ""
    echo "FATAL: Pre-sync states differ between vanilla and modified runs!"
    echo "The two runs would have different amounts of work, making comparison invalid."
    echo ""
    echo "This typically happens when the repos were not fully set up before the first"
    echo "reset. Run --setup first, then re-run the comparison."
    echo ""
    diff -u --label=pre-vanilla --label=pre-modified \
        $WORK_DIR/snapshot-pre-vanilla.txt $WORK_DIR/snapshot-pre-modified.txt
    set_color normal
    echo ""
    echo "Artifacts in $WORK_DIR"
    exit 1
end
log "Pre-sync states match — comparison is valid"

run_brancher $BINARIES[2] modified $CONFIG_DIR $WORK_DIR/modified.log
set -l modified_ms $__brancher_duration_ms
set -l modified_rc $__brancher_exit_code

log "Snapshotting destination after modified run..."
snapshot_all $WORK_DIR/snapshot-modified.txt

log "=== Phase 4: Modified no-op run (already reconciled) ==="
run_brancher $BINARIES[2] modified-noop $CONFIG_DIR $WORK_DIR/modified-noop.log
set -l modified_noop_ms $__brancher_duration_ms
set -l modified_noop_rc $__brancher_exit_code

# ── Results ──────────────────────────────────────────────────────────────────
echo ""
echo "═══════════════════════════════════════════════════════════"
echo " Results"
echo "═══════════════════════════════════════════════════════════"
echo ""

# Correctness: do both produce the same output?
compare_snapshots \
    $WORK_DIR/snapshot-vanilla.txt \
    $WORK_DIR/snapshot-modified.txt \
    vanilla modified
set -l match_status $status

echo ""

# Correctness: does each match the expected state?
verify_sync $WORK_DIR/snapshot-vanilla.txt vanilla
verify_sync $WORK_DIR/snapshot-modified.txt modified

echo ""

# Performance
report_comparison "Sync (with work to do)" $vanilla_ms $vanilla_rc $modified_ms $modified_rc
echo ""
report_comparison "No-op (already reconciled)" $vanilla_noop_ms $vanilla_noop_rc $modified_noop_ms $modified_noop_rc

echo ""
echo "Artifacts:"
echo "  $WORK_DIR/vanilla.log"
echo "  $WORK_DIR/vanilla-noop.log"
echo "  $WORK_DIR/modified.log"
echo "  $WORK_DIR/modified-noop.log"
echo "  $WORK_DIR/snapshot-pre-vanilla.txt"
echo "  $WORK_DIR/snapshot-vanilla.txt"
echo "  $WORK_DIR/snapshot-modified.txt"
echo ""
