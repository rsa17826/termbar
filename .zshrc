#!/usr/bin/env zsh
# shellcheck disable=SC1071
[[ -n "$ZSH_VERSION" ]] && zmodload zsh/datetime

TIMER_PID_FILE="/tmp/termbar_timer_${USER}_$$.pid"
TIMER_START_FILE="/tmp/termbar_timer_start_${USER}_$$.txt"

# Write to the termbar status file (no-op if not running under termbar)
function _set_status() {
  [[ -n "$TERMBAR_STATUS_FILE" ]] && echo "$1" >| "$TERMBAR_STATUS_FILE"
}

[[ -n "$TERMBAR_ENABLED" ]] && _set_status "0s 000ms"

function format_duration() {
  local delta=$1
  awk "BEGIN {
    d = $delta;
    m = int(d / 60);
    s = int(d % 60);
    ms = int((d - int(d)) * 1000);
    if (m > 0) {
      printf \"%dm %02ds %03dms\", m, s, ms;
    } else {
      printf \"%ds %03dms\", s, ms;
    }
  }"
}

function preexec() {
  if [[ -s "$TIMER_PID_FILE" ]]; then
    local old_pid=$(cat "$TIMER_PID_FILE" 2>/dev/null)
    [[ -n "$old_pid" ]] && kill -9 "$old_pid" 2>/dev/null
    rm -f "$TIMER_PID_FILE"
  fi

  local start_time=$EPOCHREALTIME
  local parent_pid=$$

  echo "$start_time" >| "$TIMER_START_FILE"
  unsetopt MONITOR 2>/dev/null

  (
    trap "exit" INT TERM EXIT
    while true; do
      kill -0 $parent_pid 2>/dev/null || exit

      local now=$EPOCHREALTIME
      local delta=$(awk "BEGIN {print $now - $start_time}")
      _set_status "$(format_duration "$delta")"

      sleep 0.05
    done
  ) >/dev/null 2>&1 &|

  echo $! >| "$TIMER_PID_FILE"
  setopt MONITOR 2>/dev/null
}

function zsh-timer-exit-cleanup() {
  if [[ -s "$TIMER_PID_FILE" ]]; then
    local _pid=$(cat "$TIMER_PID_FILE" 2>/dev/null)
    [[ -n "$_pid" ]] && kill -9 "$_pid" 2>/dev/null
    rm -f "$TIMER_PID_FILE"
  fi
  rm -f "$TIMER_START_FILE"
}
add-zsh-hook zshexit zsh-timer-exit-cleanup

function precmd() {
  local exact_end_time=$EPOCHREALTIME

  if [[ -s "$TIMER_START_FILE" && -n "$TERMBAR_ENABLED" ]]; then
    local start_time=$(cat "$TIMER_START_FILE" 2>/dev/null)
    if [[ -n "$start_time" ]]; then
      local delta=$(awk "BEGIN {print $exact_end_time - $start_time}")
      _set_status "$(format_duration "$delta")"
    fi
    rm -f "$TIMER_START_FILE"
  fi

  if [[ -s "$TIMER_PID_FILE" ]]; then
    local target_pid=$(cat "$TIMER_PID_FILE" 2>/dev/null)
    if [[ -n "$target_pid" ]]; then
      unsetopt MONITOR 2>/dev/null
      kill -9 "$target_pid" 2>/dev/null
      setopt MONITOR 2>/dev/null
    fi
    rm -f "$TIMER_PID_FILE"
  fi
}

# Auto-start termbar when in an interactive terminal that isn't already inside it
if [[ -z "$TERMBAR_ENABLED" && -n "$PS1" ]] && tty | grep -qv tty; then
  exec termbar
fi
