#!/usr/bin/env bash
# Configure an explicit supported sandbox prerequisite on disposable CI runners.
set -euo pipefail
[[ ${GITHUB_ACTIONS:-} == true && ${RUNNER_OS:-} == Linux ]] || {
  echo 'This provisioning script is only for disposable Linux GitHub runners.' >&2
  exit 2
}
id
uname -a
dpkg-query -W bubblewrap apparmor
stat -c '%U %G %a %n' /usr/bin/bwrap
sha256sum /usr/bin/bwrap
sysctl kernel.apparmor_restrict_unprivileged_userns kernel.unprivileged_userns_clone || true
sudo dmesg | tail -100 | rg 'apparmor|userns|bwrap' || true
[[ ! -L /usr/bin/bwrap && $(stat -c '%u' /usr/bin/bwrap) == 0 ]]
mode=$(stat -c '%a' /usr/bin/bwrap)
(( (8#$mode & 06022) == 0 ))
if [[ ! -e /sys/module/apparmor/parameters/enabled ]]; then
  echo 'AppArmor is not installed; no profile change.'
  exit 0
fi
cat /sys/module/apparmor/parameters/enabled
[[ $(cat /sys/module/apparmor/parameters/enabled) == Y ]] || exit 0
command -v rg >/dev/null
existing=$(sudo rg -l '/usr/bin/bwrap' /etc/apparmor.d/ || true)
if [[ -n $existing ]]; then
  echo 'Existing bubblewrap profiles are preserved; qualification tests determine compatibility.'
  while IFS= read -r profile; do
    sudo cat "$profile"
    sudo sha256sum "$profile"
  done <<< "$existing"
  exit 0
fi
profile=/etc/apparmor.d/codex-dispatch-bwrap
[[ ! -e $profile ]]
sudo tee "$profile" >/dev/null <<'PROFILE'
abi <abi/4.0>,
include <tunables/global>
profile codex-dispatch-bwrap /usr/bin/bwrap flags=(unconfined) {
  userns,
}
PROFILE
sudo apparmor_parser --replace "$profile"
sudo cat "$profile"
sudo sha256sum "$profile"
sudo cat /sys/kernel/security/apparmor/profiles | rg codex-dispatch-bwrap
