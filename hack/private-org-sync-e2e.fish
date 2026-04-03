#!/usr/bin/env fish
#
# E2E test harness for private-org-sync
#
# Syncs repos from petr-muller -> muller-testing-org, comparing a "vanilla"
# binary against a "modified" one for correctness and performance.
#
# Prerequisites:
#   - gh CLI authenticated with push access to muller-testing-org
#   - GitHub token file readable by the binary (for git push via HTTPS)
#   - Destination repos already created in muller-testing-org (empty is fine)
#   - Both binaries built beforehand
#
# Usage:
#   ./private-org-sync-e2e.fish <vanilla-binary> <modified-binary>
#   ./private-org-sync-e2e.fish --bootstrap          # initial sync to populate destinations
#   ./private-org-sync-e2e.fish --prune-only          # just prune destinations, don't run sync
#   ./private-org-sync-e2e.fish --single <binary>     # single run (no comparison)

# ── Configuration ────────────────────────────────────────────────────────────

set SOURCE_ORG petr-muller
set TARGET_ORG muller-testing-org
set TOKEN_PATH ~/.config/private-org-sync/token
set GIT_NAME "Petr Muller"
set GIT_EMAIL "afri@afri.cz"

# Small repos — full prune: delete all non-default branches, reset default
#   wetware                  (25 branches,  616 KB)
#   rh-op-ecosystem-release  (24 branches,  113 KB)
#   ota                      ( 3 branches,  356 KB)
#   pyff                     ( 2 branches,  136 KB)
#   smg                      ( 1 branch,    297 KB)
#   fucc                     ( 1 branch,    228 KB)
#   tron                     ( 1 branch,    172 KB)
#   confwatch                ( 1 branch,    120 KB)
set FULL_PRUNE_REPOS \
    wetware \
    rh-op-ecosystem-release \
    ota \
    pyff \
    smg \
    fucc \
    tron \
    confwatch

# Larger repos — light prune: delete a few branches, reset a few, leave the rest
#   cluster-version-operator (63 branches,   39 MB)
#   cincinnati-graph-data    (80 branches,   11 MB)
#   ocp-build-data           (62 branches,    9 MB)
#   prow                     (43 branches,   39 MB)
set LIGHT_PRUNE_REPOS \
    cluster-version-operator \
    cincinnati-graph-data \
    ocp-build-data \
    prow

# All repos (union of both lists)
set REPOS $FULL_PRUNE_REPOS $LIGHT_PRUNE_REPOS

# Full prune: how many commits to roll the default branch back
set FULL_RESET_DEPTH 5

# Light prune parameters
set LIGHT_DELETE_COUNT 3  # branches to delete entirely
set LIGHT_RESET_COUNT 4   # branches to reset to an older commit
set LIGHT_RESET_DEPTH 3   # how many commits to roll those branches back

# Branches to always delete during prune (orphan branches that trigger fetch retries)
set RETRY_TEST_REPO cincinnati-graph-data
set RETRY_TEST_BRANCHES test-retry-4 test-retry-16 test-retry-64

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

function create_whitelist
    set -l file $argv[1]
    begin
        echo "whitelist:"
        echo "  $SOURCE_ORG:"
        for repo in $REPOS
            echo "    - $repo"
        end
    end >$file
    log "Whitelist written to $file"
end

# URL-encode a branch name for GitHub API ref paths (handles slashes)
function encode_ref
    string replace -a / '%2F' -- $argv[1]
end

# ── Pre-flight checks ───────────────────────────────────────────────────────

function preflight
    # Check gh CLI
    if not command -q gh
        log_error "gh CLI not found"
        return 1
    end

    # Check token file
    if not test -f $TOKEN_PATH
        log_error "Token file not found at $TOKEN_PATH"
        echo "  Create it with: mkdir -p (dirname $TOKEN_PATH) && echo YOUR_TOKEN > $TOKEN_PATH" >&2
        return 1
    end

    # Check destination repos exist (with retry for transient timeouts)
    set -l missing 0
    for repo in $REPOS
        set -l found false
        for attempt in 1 2 3
            if gh api repos/$TARGET_ORG/$repo --silent 2>/dev/null
                set found true
                break
            end
            test $attempt -lt 3; and sleep 2
        end
        if not $found
            log_error "Destination repo $TARGET_ORG/$repo does not exist"
            set missing (math $missing + 1)
        end
    end
    if test $missing -gt 0
        log_error "$missing destination repo(s) missing — create them in $TARGET_ORG first"
        return 1
    end

    log "Pre-flight checks passed"
    return 0
end

# ── Snapshot: record branch -> HEAD SHA for all destination repos ────────────

function snapshot_repo
    set -l org $argv[1]
    set -l repo $argv[2]
    set -l outfile $argv[3]
    for attempt in 1 2 3
        set -l lines (gh api repos/$org/$repo/branches --paginate \
            -q '.[] | "\(.name) \(.commit.sha)"' 2>/dev/null | sort)
        if test (count $lines) -gt 0
            for line in $lines
                echo "$repo $line" >>$outfile
            end
            return 0
        end
        if test $attempt -lt 3
            log_error "  snapshot $org/$repo failed (attempt $attempt), retrying..."
            sleep 2
        end
    end
    log_error "  snapshot $org/$repo failed after 3 attempts (0 branches returned)"
    return 1
end

function snapshot_source
    set -l outfile $argv[1]
    true >$outfile
    set -l failed 0
    for repo in $REPOS
        log "  source snapshot: $SOURCE_ORG/$repo"
        if not snapshot_repo $SOURCE_ORG $repo $outfile
            set failed (math $failed + 1)
        end
    end
    if test $failed -gt 0
        log_error "Source snapshot incomplete: $failed repo(s) failed"
        return 1
    end
end

function snapshot_destination
    set -l outfile $argv[1]
    true >$outfile
    set -l failed 0
    for repo in $REPOS
        log "  destination snapshot: $TARGET_ORG/$repo"
        if not snapshot_repo $TARGET_ORG $repo $outfile
            set failed (math $failed + 1)
        end
    end
    if test $failed -gt 0
        log_error "Destination snapshot incomplete: $failed repo(s) failed"
        return 1
    end
end

# ── Prune: reset destination repos so the sync has work to do ────────────────

# Delete a branch in the target org via the GitHub API
function delete_branch
    set -l repo $argv[1]
    set -l branch $argv[2]
    set -l encoded (encode_ref $branch)
    for attempt in 1 2 3
        set -l output (gh api -X DELETE "repos/$TARGET_ORG/$repo/git/refs/heads/$encoded" 2>&1)
        set -l rc $status
        if test $rc -eq 0
            return 0
        end
        if test $attempt -lt 3
            sleep 2
        else
            log_error "    delete $branch (after $attempt attempts): $output"
            return $rc
        end
    end
end

# Force-reset a branch to an older commit (N commits back)
# Falls back to fewer commits if the branch is short, skips if only 1 commit
function reset_branch
    set -l repo $argv[1]
    set -l branch $argv[2]
    set -l depth $argv[3]

    # Fetch depth+1 commits so we can pick one that's actually behind HEAD
    set -l fetch_count (math $depth + 1)
    set -l shas (
        gh api "repos/$TARGET_ORG/$repo/commits" \
            -f sha=$branch \
            -F per_page=$fetch_count \
            -q '.[].sha' 2>/dev/null
    )
    if test (count $shas) -lt 2
        # Branch has only one commit, nothing to reset to
        return 1
    end

    # Pick the last SHA (oldest available), which is guaranteed != HEAD
    set -l old_sha $shas[-1]

    set -l encoded (encode_ref $branch)
    gh api -X PATCH "repos/$TARGET_ORG/$repo/git/refs/heads/$encoded" \
        -f sha=$old_sha -F force=true --silent 2>/dev/null
end

# Full prune: delete all non-default branches, reset default branch
function prune_full
    set -l repo $argv[1]
    set -l default_branch $argv[2]
    set -l branches $argv[3..]
    set -l deleted 0
    set -l reset 0

    for branch in $branches
        if test "$branch" = "$default_branch"
            continue
        end
        if delete_branch $repo $branch
            set deleted (math $deleted + 1)
        else
            log_error "    failed to delete branch $branch"
        end
    end

    if reset_branch $repo $default_branch $FULL_RESET_DEPTH
        set reset 1
        log "    full prune: deleted $deleted branches, reset default"
    else
        log "    full prune: deleted $deleted branches, default has too few commits to reset"
    end
    set -g __prune_deleted $deleted
    set -g __prune_reset $reset
end

# Light prune: delete a few branches, reset a few others, leave the rest
# Uses source branches (argv[3..]) for deterministic selection — always targets
# the same branches regardless of the destination's current state.
function prune_light
    set -l repo $argv[1]
    set -l default_branch $argv[2]
    set -l source_branches $argv[3..]
    set -l deleted 0
    set -l reset 0

    # Use source branches (sorted, non-default) as candidates for deterministic selection
    set -l candidates
    for branch in $source_branches
        if test "$branch" != "$default_branch"
            set -a candidates $branch
        end
    end

    if test (count $candidates) -eq 0
        log "    light prune: no non-default branches to prune"
        set -g __prune_deleted 0
        set -g __prune_reset 0
        return
    end

    # Delete the first N candidates
    set -l to_delete (math "min($LIGHT_DELETE_COUNT, "(count $candidates)")")
    for i in (seq 1 $to_delete)
        if delete_branch $repo $candidates[$i]
            set deleted (math $deleted + 1)
            log "    deleted: $candidates[$i]"
        else
            log "    skip delete: $candidates[$i] (not in destination)"
        end
    end

    # Reset the next M candidates (after the deleted ones)
    set -l reset_start (math $to_delete + 1)
    set -l reset_end (math "min($reset_start + $LIGHT_RESET_COUNT - 1, "(count $candidates)")")
    for i in (seq $reset_start $reset_end)
        if reset_branch $repo $candidates[$i] $LIGHT_RESET_DEPTH
            set reset (math $reset + 1)
            log "    reset: $candidates[$i] ($LIGHT_RESET_DEPTH commits back)"
        else
            log "    skip reset: $candidates[$i] (too few commits or not in destination)"
        end
    end

    set -l reset_attempted (math "$reset_end - $reset_start + 1")
    test $reset_attempted -lt 0; and set reset_attempted 0
    set -l untouched (math (count $candidates) - $to_delete - $reset_attempted)
    log "    light prune: deleted $deleted, reset $reset, left $untouched untouched"
    set -g __prune_deleted $deleted
    set -g __prune_reset $reset
end

function prune_destination
    set -l source_snapshot $argv[1]
    log "Pruning destination repos..."
    set -l total_deleted 0
    set -l total_reset 0

    for repo in $REPOS
        # Determine prune mode for this repo
        set -l mode full
        if contains $repo $LIGHT_PRUNE_REPOS
            set mode light
        end

        log "  pruning $TARGET_ORG/$repo ($mode)"

        # Get default branch (with retry)
        set -l default_branch
        for attempt in 1 2 3
            set default_branch (gh api repos/$TARGET_ORG/$repo -q '.default_branch' 2>/dev/null)
            test -n "$default_branch"; and break
            test $attempt -lt 3; and sleep 2
        end
        if test -z "$default_branch"
            log_error "    could not determine default branch after 3 attempts, skipping"
            continue
        end

        if test "$mode" = light
            # Use source branches for deterministic selection (same branches every run)
            set -l source_branches (grep "^$repo " $source_snapshot | awk '{print $2}' | sort)
            prune_light $repo $default_branch $source_branches
        else
            # Full prune: delete all destination branches (need current list)
            set -l branches (gh api repos/$TARGET_ORG/$repo/branches --paginate -q '.[].name' 2>/dev/null | sort)
            prune_full $repo $default_branch $branches
        end

        set total_deleted (math $total_deleted + $__prune_deleted)
        set total_reset (math $total_reset + $__prune_reset)
    end

    # Always delete retry-test branches to force fetch retries during sync
    # (these are orphan branches with no shared objects on the destination)
    for branch in $RETRY_TEST_BRANCHES
        if delete_branch $RETRY_TEST_REPO $branch
            set total_deleted (math $total_deleted + 1)
            log "  deleted retry-test branch: $RETRY_TEST_REPO/$branch"
        end
    end

    log "Pruning done: $total_deleted branches deleted, $total_reset branches reset"
end

# ── Run the sync binary ─────────────────────────────────────────────────────

function run_sync
    set -l binary $argv[1]
    set -l label $argv[2]
    set -l whitelist_file $argv[3]
    set -l config_dir $argv[4]
    set -l logfile $argv[5]

    log "Running sync [$label]: $binary"

    set -l start_ns (date +%s%N)

    $binary \
        --token-path=$TOKEN_PATH \
        --config-dir=$config_dir \
        --target-org=$TARGET_ORG \
        --whitelist-file=$whitelist_file \
        --flatten-org=$SOURCE_ORG \
        --git-name=$GIT_NAME \
        --git-email=$GIT_EMAIL \
        --confirm \
        --log-level=debug \
        2>&1 | tee $logfile

    set -l rc $pipestatus[1]
    set -l end_ns (date +%s%N)
    set -l duration_ms (math "($end_ns - $start_ns) / 1000000")

    log "[$label] finished in {$duration_ms}ms (exit code $rc)"

    # Return duration via a global so caller can capture it
    set -g __sync_duration_ms $duration_ms
    set -g __sync_exit_code $rc
    return $rc
end

# ── Compare two snapshots ────────────────────────────────────────────────────

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

function compare_to_source
    set -l source_file $argv[1]
    set -l dest_file $argv[2]
    set -l label $argv[3]

    if diff -q $source_file $dest_file >/dev/null 2>&1
        set_color green
        echo "PASS: [$label] destination matches source exactly"
        set_color normal
        return 0
    else
        set_color red
        echo "DIFF: [$label] destination does not match source:"
        set_color normal
        diff -u --label=source --label="$label" $source_file $dest_file
        return 1
    end
end

# ── Main ─────────────────────────────────────────────────────────────────────

# Parse mode
set MODE compare
set BINARIES

switch "$argv[1]"
    case --bootstrap
        set MODE bootstrap
        if test (count $argv) -lt 2
            echo "Usage: $0 --bootstrap <binary>"
            exit 1
        end
        set BINARIES $argv[2]
    case --prune-only
        set MODE prune
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
        echo "  $0 --bootstrap <binary>                 Initial sync to populate destinations"
        echo "  $0 --prune-only                         Just prune destinations"
        echo "  $0 --single <binary>                    Single run, verify against source"
        echo ""
        echo "Configuration (edit the script to change):"
        echo "  SOURCE_ORG        = $SOURCE_ORG"
        echo "  TARGET_ORG        = $TARGET_ORG"
        echo "  TOKEN_PATH        = $TOKEN_PATH"
        echo "  FULL_PRUNE_REPOS  = $FULL_PRUNE_REPOS"
        echo "  LIGHT_PRUNE_REPOS = $LIGHT_PRUNE_REPOS"
        echo "  FULL_RESET_DEPTH  = $FULL_RESET_DEPTH"
        echo "  LIGHT params      = delete $LIGHT_DELETE_COUNT, reset $LIGHT_RESET_COUNT by $LIGHT_RESET_DEPTH"
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
set WORK_DIR (mktemp -d /tmp/private-org-sync-e2e.XXXXXX)
set CONFIG_DIR $WORK_DIR/config
set WHITELIST $WORK_DIR/whitelist.yaml
mkdir -p $CONFIG_DIR

log "Work directory: $WORK_DIR"

# Create whitelist
create_whitelist $WHITELIST

# Snapshot source (once — it doesn't change between runs)
log "Snapshotting source repos..."
if not snapshot_source $WORK_DIR/snapshot-source.txt
    log_error "Failed to snapshot source repos — aborting"
    exit 1
end
set -l source_branches (wc -l <$WORK_DIR/snapshot-source.txt | string trim)
log "Source has $source_branches branch(es) across "(count $REPOS)" repo(s)"

# ── Mode: prune-only ────────────────────────────────────────────────────────
if test "$MODE" = prune
    prune_destination $WORK_DIR/snapshot-source.txt
    log "Done. Destination repos have been pruned."
    exit 0
end

# ── Mode: bootstrap ─────────────────────────────────────────────────────────
if test "$MODE" = bootstrap
    log "=== Bootstrap: initial sync to populate destinations ==="
    run_sync $BINARIES[1] bootstrap $WHITELIST $CONFIG_DIR $WORK_DIR/bootstrap.log

    log "Snapshotting destination after bootstrap..."
    snapshot_destination $WORK_DIR/snapshot-bootstrap.txt
    compare_to_source $WORK_DIR/snapshot-source.txt $WORK_DIR/snapshot-bootstrap.txt bootstrap

    log "Done. Logs in $WORK_DIR"
    exit 0
end

# ── Mode: single ────────────────────────────────────────────────────────────
if test "$MODE" = single
    log "=== Single run ==="
    prune_destination $WORK_DIR/snapshot-source.txt

    run_sync $BINARIES[1] single $WHITELIST $CONFIG_DIR $WORK_DIR/single.log
    set -l duration $__sync_duration_ms

    log "Snapshotting destination..."
    snapshot_destination $WORK_DIR/snapshot-single.txt
    compare_to_source $WORK_DIR/snapshot-source.txt $WORK_DIR/snapshot-single.txt single

    echo ""
    log "Duration: {$duration}ms"
    log "Logs in $WORK_DIR"
    exit 0
end

# ── Mode: compare (default) ─────────────────────────────────────────────────

# Ensure destination has all source branches before pruning.
# Without this, branches that exist in source but not in destination will be
# synced by the vanilla run but not cleaned up by the second prune, causing
# a pre-state mismatch. This can happen when new branches are pushed to the
# source repos between bootstraps.
log "Reconciling destination with source (ensuring all source branches exist)..."
set -l missing_count 0
for repo in $REPOS
    set -l dst_branches (gh api repos/$TARGET_ORG/$repo/branches --paginate -q '.[].name' 2>/dev/null | sort)
    set -l src_branches (grep "^$repo " $WORK_DIR/snapshot-source.txt | awk '{print $2}' | sort)
    for branch in $src_branches
        if not contains $branch $dst_branches
            set missing_count (math $missing_count + 1)
        end
    end
end
if test $missing_count -gt 0
    log "$missing_count source branch(es) missing from destination — running bootstrap sync"
    run_sync $BINARIES[1] bootstrap-reconcile $WHITELIST $CONFIG_DIR $WORK_DIR/bootstrap-reconcile.log
    log "Bootstrap reconciliation complete"
else
    log "Destination already has all source branches"
end

log "=== Phase 1: Vanilla run ==="
prune_destination $WORK_DIR/snapshot-source.txt

log "Snapshotting destination after prune (pre-vanilla)..."
snapshot_destination $WORK_DIR/snapshot-pre-vanilla.txt

run_sync $BINARIES[1] vanilla $WHITELIST $CONFIG_DIR $WORK_DIR/vanilla.log
set -l vanilla_ms $__sync_duration_ms
set -l vanilla_rc $__sync_exit_code

log "Snapshotting destination after vanilla run..."
snapshot_destination $WORK_DIR/snapshot-vanilla.txt

log "=== Phase 2: Vanilla no-op run (already reconciled) ==="
run_sync $BINARIES[1] vanilla-noop $WHITELIST $CONFIG_DIR $WORK_DIR/vanilla-noop.log
set -l vanilla_noop_ms $__sync_duration_ms
set -l vanilla_noop_rc $__sync_exit_code

log "=== Phase 3: Modified run ==="
prune_destination $WORK_DIR/snapshot-source.txt

log "Snapshotting destination after prune (pre-modified)..."
snapshot_destination $WORK_DIR/snapshot-pre-modified.txt

# Validate both runs start from the same state
if not diff -q $WORK_DIR/snapshot-pre-vanilla.txt $WORK_DIR/snapshot-pre-modified.txt >/dev/null 2>&1
    set_color red
    echo ""
    echo "FATAL: Pre-sync states differ between vanilla and modified runs!"
    echo "The two runs would have different amounts of work, making comparison invalid."
    echo ""
    echo "This typically happens when the destination repos were not fully synced"
    echo "before the first prune. Run --bootstrap first, then re-run the comparison."
    echo ""
    diff -u --label=pre-vanilla --label=pre-modified \
        $WORK_DIR/snapshot-pre-vanilla.txt $WORK_DIR/snapshot-pre-modified.txt
    set_color normal
    echo ""
    echo "Artifacts in $WORK_DIR"
    exit 1
end
log "Pre-sync states match — comparison is valid"

run_sync $BINARIES[2] modified $WHITELIST $CONFIG_DIR $WORK_DIR/modified.log
set -l modified_ms $__sync_duration_ms
set -l modified_rc $__sync_exit_code

log "Snapshotting destination after modified run..."
snapshot_destination $WORK_DIR/snapshot-modified.txt

log "=== Phase 4: Modified no-op run (already reconciled) ==="
run_sync $BINARIES[2] modified-noop $WHITELIST $CONFIG_DIR $WORK_DIR/modified-noop.log
set -l modified_noop_ms $__sync_duration_ms
set -l modified_noop_rc $__sync_exit_code

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

# Correctness: does each match the source?
compare_to_source $WORK_DIR/snapshot-source.txt $WORK_DIR/snapshot-vanilla.txt vanilla
compare_to_source $WORK_DIR/snapshot-source.txt $WORK_DIR/snapshot-modified.txt modified

echo ""

# Performance helper
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

report_comparison "Sync (with work to do)" $vanilla_ms $vanilla_rc $modified_ms $modified_rc
echo ""
report_comparison "No-op (already reconciled)" $vanilla_noop_ms $vanilla_noop_rc $modified_noop_ms $modified_noop_rc

echo ""
echo "Artifacts:"
echo "  $WORK_DIR/vanilla.log"
echo "  $WORK_DIR/vanilla-noop.log"
echo "  $WORK_DIR/modified.log"
echo "  $WORK_DIR/modified-noop.log"
echo "  $WORK_DIR/snapshot-source.txt"
echo "  $WORK_DIR/snapshot-vanilla.txt"
echo "  $WORK_DIR/snapshot-modified.txt"
echo ""
