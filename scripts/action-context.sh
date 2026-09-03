#!/usr/bin/env bash
set -euo pipefail

# Env: COMMAND MODE DRY_RUN APPLY_ON ALLOWED_ASSOCIATIONS EVENT_NAME EVENT_PATH REPOSITORY GH_TOKEN GITHUB_SHA GITHUB_OUTPUT RUN_URL
out() { printf '%s=%s\n' "$1" "$2" >>"${GITHUB_OUTPUT}"; }
emit() {
  out skipped "$1"
  out pr-number "$2"
  out head-sha "$3"
}
skip() {
  echo "::notice::godwit: $1"
  emit true "" ""
  exit 0
}
refuse() {
  echo "::error::godwit: $1"
  exit "${2:-1}"
}
event() { jq -r "$1" "${EVENT_PATH}"; }

case "${COMMAND}" in
  lint|plan|migrate|apply|verify|revert) ;;
  *) refuse "unknown command '${COMMAND}' (want lint, plan, migrate, apply, verify or revert)" 2 ;;
esac
case "${MODE}" in
  apply-on-pr)
    if [ "${COMMAND}" = "migrate" ] && [ "${DRY_RUN}" != "true" ]; then
      refuse "mode apply-on-pr never applies outside the pull request: use command: apply on the pull request and command: verify on push, or mode: apply-on-merge" 2
    fi
    ;;
  apply-on-merge)
    case "${COMMAND}" in
      apply|revert) refuse "command ${COMMAND} needs mode: apply-on-pr (mode apply-on-merge applies with command: migrate on push)" 2 ;;
    esac
    ;;
  *) refuse "unknown mode '${MODE}' (want apply-on-pr or apply-on-merge)" 2 ;;
esac

case "${COMMAND}" in
  apply|revert) ;;
  *)
    case "${EVENT_NAME}" in
      pull_request|pull_request_target) emit false "$(event .pull_request.number)" "$(event .pull_request.head.sha)" ;;
      *) emit false "" "${GITHUB_SHA}" ;;
    esac
    exit 0
    ;;
esac

want="/godwit ${COMMAND}"
commanded() {
  printf '%s\n' "$1" | tr -d '\r' | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' | grep -qxF "${want}"
}
allowed() {
  case ",${ALLOWED_ASSOCIATIONS// /}," in
    *",$1,"*) return 0 ;;
  esac
  refuse "${want} by ${2} refused: author association ${1} is not in ${ALLOWED_ASSOCIATIONS}"
}
wants_on() {
  case ",${APPLY_ON// /}," in
    *",$1,"*) return 0 ;;
  esac

  return 1
}
if ! wants_on comment && ! wants_on approve; then
  refuse "apply-on '${APPLY_ON}' enables nothing (want comment, approve or comment,approve)" 2
fi
by_comment() {
  if ! commanded "$1"; then skip "$2 is not '${want}'"; fi
  if [ "${COMMAND}" = "apply" ] && ! wants_on comment; then skip "apply-on: ${APPLY_ON} ignores '${want}' comments"; fi
}

case "${EVENT_NAME}" in
  issue_comment)
    if [ "$(event .action)" != "created" ]; then skip "comment ${EVENT_NAME} $(event .action), nothing to do"; fi
    if [ "$(event '.issue.pull_request // empty')" = "" ]; then skip "comment on an issue, not a pull request"; fi
    by_comment "$(event '.comment.body // ""')" comment
    allowed "$(event '.comment.author_association // "NONE"')" "$(event .comment.user.login)"
    number="$(event .issue.number)"
    ;;
  pull_request_review)
    if [ "$(event .action)" != "submitted" ]; then skip "review ${EVENT_NAME} $(event .action), nothing to do"; fi
    if [ "${COMMAND}" = "apply" ] && wants_on approve && [ "$(event .review.state)" = "approved" ]; then
      echo "godwit: approved review, applying"
    else
      by_comment "$(event '.review.body // ""')" review
    fi
    allowed "$(event '.review.author_association // "NONE"')" "$(event .review.user.login)"
    number="$(event .pull_request.number)"
    ;;
  pull_request|pull_request_target)
    number="$(event .pull_request.number)"
    ;;
  *)
    refuse "command ${COMMAND} runs on issue_comment, pull_request_review or pull_request events, not ${EVENT_NAME}"
    ;;
esac

pr="$(gh api "repos/${REPOSITORY}/pulls/${number}")"
head="$(printf '%s' "${pr}" | jq -r .head.sha)"
state="$(printf '%s' "${pr}" | jq -r .state)"
merged="$(printf '%s' "${pr}" | jq -r .merged)"
if [ -z "${head}" ] || [ "${head}" = "null" ]; then
  refuse "could not read the head of pull request #${number}"
fi

case "${COMMAND}" in
  apply)
    if [ "${state}" != "open" ]; then
      refuse "pull request #${number} is ${state}: nothing to apply"
    fi
    checked_out="$(git rev-parse HEAD 2>/dev/null || true)"
    if [ "${checked_out}" != "${head}" ]; then
      refuse "checked-out commit ${checked_out:-<none>} is not the head of pull request #${number} (${head}); check out the head (ref: refs/pull/${number}/head) and, if it moved after the command, comment ${want} again"
    fi
    STATE=pending DESCRIPTION="applying ${head:0:7} from pull request #${number}" TARGET_URL="${RUN_URL}" SHA="${head}" \
      "$(dirname "$0")/action-status.sh"
    ;;
  revert)
    if [ "${merged}" = "true" ]; then
      refuse "pull request #${number} was merged: its migrations belong to the base branch now, revert them from a new pull request"
    fi
    ;;
esac

echo "godwit: ${want} on pull request #${number} at ${head}"
emit false "${number}" "${head}"
