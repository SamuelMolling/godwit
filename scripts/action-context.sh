#!/usr/bin/env bash
set -euo pipefail

# Env: COMMAND MODE DRY_RUN APPLY_ON ALLOWED_ASSOCIATIONS REQUIRE_APPROVAL EVENT_NAME EVENT_PATH REPOSITORY GH_TOKEN GITHUB_SHA GITHUB_OUTPUT RUN_URL
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
hex() { [ -n "$1" ] && [ -z "${1//[0-9a-f]/}" ]; }

case "${COMMAND}" in
  lint|plan|migrate|apply|confirm|verify|revert|diff) ;;
  *) refuse "unknown command '${COMMAND}' (want lint, plan, migrate, apply, confirm, verify, revert or diff)" 2 ;;
esac
case "${MODE}" in
  apply-on-pr)
    if [ "${COMMAND}" = "migrate" ] && [ "${DRY_RUN}" != "true" ]; then
      refuse "mode apply-on-pr never applies outside the pull request: use command: apply on the pull request and command: verify on push, or mode: apply-on-merge" 2
    fi
    ;;
  apply-on-merge)
    case "${COMMAND}" in
      apply|confirm|revert) refuse "command ${COMMAND} needs mode: apply-on-pr (mode apply-on-merge applies with command: migrate on push and confirms with godwit run confirm --latest)" 2 ;;
    esac
    ;;
  *) refuse "unknown mode '${MODE}' (want apply-on-pr or apply-on-merge)" 2 ;;
esac

writes=false
case "${COMMAND}" in
  apply|confirm|revert) writes=true ;;
  migrate) if [ "${DRY_RUN}" != "true" ]; then writes=true; fi ;;
esac

pr_number=""
pr_head=""
case "${EVENT_NAME}" in
  pull_request|pull_request_target)
    pr_number="$(event '.pull_request.number // ""')"
    pr_head="$(event '.pull_request.head.sha // ""')"
    head_repo="$(event '.pull_request.head.repo.full_name // ""')"
    if [ -z "${pr_number}" ] || [ -n "${pr_number//[0-9]/}" ]; then
      refuse "the ${EVENT_NAME} payload carries no pull request number"
    fi
    if ! hex "${pr_head}" || [ "${#pr_head}" -ne 40 ]; then
      refuse "the ${EVENT_NAME} payload carries no head commit"
    fi
    if [ "${EVENT_NAME}" = "pull_request_target" ] && [ "${writes}" = "true" ]; then
      refuse "command ${COMMAND} is refused on pull_request_target: that event runs in ${REPOSITORY} with its secrets and a write token even for a pull request opened from a fork, so whoever opened it would apply their own migrations; run this command on pull_request, which withholds the secrets from forks, or command it with a '/godwit ${COMMAND}' comment on the pull request" 2
    fi
    if [ "${head_repo}" != "${REPOSITORY}" ]; then
      if [ "${EVENT_NAME}" = "pull_request_target" ]; then
        refuse "pull_request_target for a pull request from ${head_repo:-an unknown repository} is refused: the job would run that fork's checkout with ${REPOSITORY}'s secrets; trigger on pull_request instead"
      fi
      if [ "${writes}" = "true" ]; then
        refuse "command ${COMMAND} is refused on a pull request from ${head_repo:-an unknown repository}: a fork may not apply to the targets of ${REPOSITORY}; review it and comment /godwit ${COMMAND} instead"
      fi
    fi
    ;;
esac

case "${COMMAND}" in
  apply|confirm|revert) ;;
  *)
    case "${EVENT_NAME}" in
      pull_request|pull_request_target) emit false "${pr_number}" "${pr_head}" ;;
      *) emit false "" "${GITHUB_SHA}" ;;
    esac
    exit 0
    ;;
esac

case "${REQUIRE_APPROVAL}" in
  true|false) ;;
  *) refuse "unknown require-approval '${REQUIRE_APPROVAL}' (want true or false)" 2 ;;
esac
associations="${ALLOWED_ASSOCIATIONS// /}"
if [ -z "${associations}" ]; then
  refuse "allowed-associations is empty: no comment could ever command ${COMMAND}" 2
fi
for association in ${associations//,/ }; do
  case "${association}" in
    OWNER|MEMBER|COLLABORATOR) ;;
    CONTRIBUTOR|FIRST_TIME_CONTRIBUTOR|FIRST_TIMER|MANNEQUIN|NONE)
      refuse "allowed-associations must not contain ${association}: it is not access to ${REPOSITORY}, anyone who opened a pull request carries it" 2 ;;
    *) refuse "allowed-associations carries an unknown value '${association}' (want OWNER, MEMBER or COLLABORATOR)" 2 ;;
  esac
done

want="/godwit ${COMMAND}"
command_sha=""
# A whole line outside a fenced block, so a pasted log carrying the command does not fire.
commanded() {
  local line rest fenced=0
  while IFS= read -r line; do
    line="${line%$'\r'}"
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    case "${line}" in
      '```'*|'~~~'*)
        fenced=$((1 - fenced))
        continue
        ;;
    esac
    if [ "${fenced}" -eq 1 ]; then continue; fi
    if [ "${line}" = "${want}" ]; then
      command_sha=""

      return 0
    fi
    case "${line}" in
      "${want} "*)
        rest="${line#"${want} "}"
        if hex "${rest}" && [ "${#rest}" -ge 7 ] && [ "${#rest}" -le 40 ]; then
          command_sha="${rest}"

          return 0
        fi
        ;;
    esac
  done <<<"$1"

  return 1
}
allowed() {
  case ",${associations}," in
    *",$1,"*) return 0 ;;
  esac
  refuse "${want} by ${2} refused: author association ${1} is not in ${ALLOWED_ASSOCIATIONS}"
}
# author_association is a relationship with the owning org, not access to this repository.
permitted() {
  local login="$1" role="$2" perm
  if [ -z "${login}" ] || [ -n "${login//[A-Za-z0-9-]/}" ]; then
    refuse "${want} refused: '${login}' is not a GitHub login"
  fi
  if ! perm="$(gh api "repos/${REPOSITORY}/collaborators/${login}/permission" --jq .permission)"; then
    refuse "${want} refused: could not read the permission of ${login} on ${REPOSITORY}; github-token must be allowed to read the repository's collaborators"
  fi
  case "${perm}" in
    admin|write) ;;
    *) refuse "${want} refused: ${role} ${login} has permission '${perm:-none}' on ${REPOSITORY}, not write or admin" ;;
  esac
}
approved() {
  local reviews approver
  if ! reviews="$(gh api --paginate "repos/${REPOSITORY}/pulls/${number}/reviews")"; then
    refuse "${want} refused: could not read the reviews of pull request #${number}"
  fi
  approver="$(printf '%s' "${reviews}" | jq -r --arg head "${head}" --arg author "${author}" '
    [.[] | select(.state == "APPROVED" or .state == "CHANGES_REQUESTED" or .state == "DISMISSED")]
    | group_by(.user.login) | map(last)
    | map(select(.state == "APPROVED" and .commit_id == $head and .user.login != $author) | .user.login)
    | first // ""')"
  if [ -z "${approver}" ]; then
    refuse "${want} on pull request #${number} refused: no approving review by anyone other than ${author:-its author} stands on ${head:0:7}; approve that commit and command ${want} again, or set require-approval: \"false\" to apply without one"
  fi
  permitted "${approver}" approver
  echo "godwit: ${head:0:7} is approved by ${approver}"
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

commander=""
review_sha=""
case "${EVENT_NAME}" in
  issue_comment)
    if [ "$(event .action)" != "created" ]; then skip "comment ${EVENT_NAME} $(event .action), nothing to do"; fi
    if [ "$(event '.issue.pull_request // empty')" = "" ]; then skip "comment on an issue, not a pull request"; fi
    by_comment "$(event '.comment.body // ""')" comment
    commander="$(event '.comment.user.login // ""')"
    allowed "$(event '.comment.author_association // "NONE"')" "${commander}"
    number="$(event '.issue.number // ""')"
    ;;
  pull_request_review)
    if [ "$(event .action)" != "submitted" ]; then skip "review ${EVENT_NAME} $(event .action), nothing to do"; fi
    if [ "${COMMAND}" = "apply" ] && wants_on approve && [ "$(event .review.state)" = "approved" ]; then
      echo "godwit: approved review, applying"
    else
      by_comment "$(event '.review.body // ""')" review
    fi
    commander="$(event '.review.user.login // ""')"
    allowed "$(event '.review.author_association // "NONE"')" "${commander}"
    review_sha="$(event '.review.commit_id // ""')"
    number="$(event '.pull_request.number // ""')"
    ;;
  pull_request|pull_request_target)
    number="${pr_number}"
    ;;
  *)
    refuse "command ${COMMAND} runs on issue_comment, pull_request_review or pull_request events, not ${EVENT_NAME}"
    ;;
esac
if [ -z "${number}" ] || [ -n "${number//[0-9]/}" ]; then
  refuse "the ${EVENT_NAME} payload carries no pull request number"
fi
if [ -n "${commander}" ]; then
  permitted "${commander}" commander
fi

pr="$(gh api "repos/${REPOSITORY}/pulls/${number}")"
head="$(printf '%s' "${pr}" | jq -r '.head.sha // ""')"
state="$(printf '%s' "${pr}" | jq -r .state)"
merged="$(printf '%s' "${pr}" | jq -r .merged)"
author="$(printf '%s' "${pr}" | jq -r '.user.login // ""')"
if ! hex "${head}"; then
  refuse "could not read the head of pull request #${number}"
fi

case "${COMMAND}" in
  apply|confirm)
    if [ "${state}" != "open" ]; then
      refuse "pull request #${number} is ${state}: nothing to ${COMMAND}"
    fi
    if [ -n "${review_sha}" ] && [ "${review_sha}" != "${head}" ]; then
      refuse "the review is on ${review_sha:0:7} but pull request #${number} is at ${head:0:7}: the head moved after the review, so ${want} would run commits nobody reviewed; review the new head and command ${want} again"
    fi
    if [ -n "${command_sha}" ]; then
      case "${head}" in
        "${command_sha}"*) ;;
        *) refuse "${want} names ${command_sha} but pull request #${number} is at ${head:0:7}: the head moved after the comment; read the new head and comment ${want} again" ;;
      esac
    fi
    checked_out="$(git rev-parse HEAD 2>/dev/null || true)"
    if [ "${checked_out}" != "${head}" ]; then
      refuse "checked-out commit ${checked_out:-<none>} is not the head of pull request #${number} (${head}); check out the head (ref: refs/pull/${number}/head) and, if it moved after the command, comment ${want} again"
    fi
    if [ "${REQUIRE_APPROVAL}" = "true" ]; then
      approved
    fi
    doing="applying"
    if [ "${COMMAND}" = "confirm" ]; then doing="confirming the contract phase of"; fi
    STATE=pending DESCRIPTION="${doing} ${head:0:7} from pull request #${number}" TARGET_URL="${RUN_URL}" SHA="${head}" \
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
