"""Machine identity, as the relay expects it.

The relay recognises a caller by any one of three markers: the IP address it
sees, a machine id, or a disk id. The IP it derives itself; the other two are
supplied here. Sending them is what stops "one draw per hour" from resetting
whenever the user changes network.

These values leave the machine, so they are hashed by the relay on arrival and
never stored in raw form. Nothing here is read that is not needed to identify
the machine, and nothing is sent that the relay does not hash.
"""

from __future__ import annotations

import platform
import re
import shutil
import subprocess
from functools import lru_cache
from pathlib import Path

from . import __version__

# Values the relay rejects as placeholders. Sending one is worse than sending
# nothing: it would hash into a marker that never matches again.
_PLACEHOLDERS = {
    "",
    "unknown",
    "none",
    "null",
    "default string",
    "to be filled by o.e.m.",
    "00000000-0000-0000-0000-000000000000",
}

_UUID_RE = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")


@lru_cache(maxsize=1)
def machine_id() -> str:
    """A stable identifier for this machine, or "" if none can be found.

    Tries, in order: the DMI product UUID (Linux, needs root for full detail but
    the world-readable copy is usually enough), macOS hardware UUID, and the
    Windows MachineGuid. Returns "" rather than a guess, because a wrong value
    is a marker that will never match again.
    """
    for reader in (_linux_machine_id, _darwin_machine_id, _windows_machine_id):
        try:
            value = reader()
        except Exception:  # noqa: BLE001 - any failure means "try the next one"
            continue
        if _usable(value):
            return value.lower()
    return ""


@lru_cache(maxsize=1)
def disk_id() -> str:
    """A stable identifier for the boot disk, or "".

    Only the root filesystem's identifier is used, so adding an external drive
    does not change it.
    """
    for reader in (_linux_disk_id, _darwin_disk_id, _windows_disk_id):
        try:
            value = reader()
        except Exception:  # noqa: BLE001
            continue
        if _usable(value):
            return value.lower()
    return ""


def _usable(value: str | None) -> bool:
    if not value:
        return False
    v = value.strip().lower()
    if v in _PLACEHOLDERS:
        return False
    if len(v) < 8 or len(v) > 128:
        return False
    # All-zero and all-dash values are placeholders however they are formatted.
    if not v.strip("0-") or not v.strip("-"):
        return False
    if not re.fullmatch(r"[a-z0-9:_\-]+", v):
        return False
    return True


def _read(path: str) -> str:
    try:
        return Path(path).read_text(encoding="utf-8", errors="replace").strip()
    except OSError:
        return ""


def _run(cmd: list[str]) -> str:
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=5, check=False)
    except (OSError, subprocess.SubprocessError):
        return ""
    return out.stdout.strip() if out.returncode == 0 else ""


def _linux_machine_id() -> str:
    for path in ("/sys/class/dmi/id/product_uuid", "/etc/machine-id", "/var/lib/dbus/machine-id"):
        value = _read(path)
        if value:
            return value
    return ""


def _darwin_machine_id() -> str:
    out = _run(["ioreg", "-rd1", "-c", "IOPlatformExpertDevice"])
    match = re.search(r'"IOPlatformUUID"\s*=\s*"([^"]+)"', out)
    return match.group(1) if match else ""


def _windows_machine_id() -> str:
    if platform.system() != "Windows":
        return ""
    out = _run(["reg", "query", r"HKLM\SOFTWARE\Microsoft\Cryptography", "/v", "MachineGuid"])
    match = re.search(r"MachineGuid\s+REG_SZ\s+(\S+)", out)
    return match.group(1) if match else ""


def _linux_disk_id() -> str:
    findmnt = shutil.which("findmnt")
    if not findmnt:
        return ""
    source = _run([findmnt, "-n", "-o", "SOURCE", "/"])
    if not source:
        return ""
    # Prefer the filesystem UUID: it survives a reinstall of the bootloader and
    # is what /dev/disk/by-uuid is keyed on.
    blkid = shutil.which("blkid")
    if blkid:
        uuid = _run([blkid, "-s", "UUID", "-o", "value", source])
        if uuid:
            return uuid
    real = Path("/dev/disk/by-uuid")
    if real.is_dir():
        for link in real.iterdir():
            try:
                if Path(link).resolve() == Path(source).resolve():
                    return link.name
            except OSError:
                continue
    return ""


def _darwin_disk_id() -> str:
    out = _run(["diskutil", "info", "-plist", "/"])
    match = re.search(r"<key>VolumeUUID</key>\s*<string>([^<]+)</string>", out)
    return match.group(1) if match else ""


def _windows_disk_id() -> str:
    if platform.system() != "Windows":
        return ""
    out = _run(["cmd", "/c", "vol", "C:"])
    match = re.search(r"([0-9A-Fa-f]{4}-[0-9A-Fa-f]{4})", out)
    return match.group(1) if match else ""


def device_class() -> str:
    """A coarse guess at what kind of machine this is: "server" or "desktop".

    The relay reads this as context, so a wrong answer is worse than a vague
    one; only a positive signal produces "server".
    """
    probes = (
        ("/sys/devices/virtual/dmi/id/chassis_type", _server_chassis),
        ("/sys/class/dmi/id/chassis_type", _server_chassis),
        ("/sys/class/dmi/id/product_name", _server_product),
    )
    for path, reader in probes:
        verdict = reader(_read(path))
        if verdict:
            return verdict
    if platform.system() == "Darwin":
        return "desktop"
    return ""


def hypervisor_present() -> bool:
    """Whether this machine reports itself as a guest.

    Sent as a positive signal only. The relay scores a hypervisor as a small
    privacy risk, so claiming one when there is none would be a small lie with a
    real cost; there is nothing to gain by making it up.
    """
    cpuinfo = _read("/proc/cpuinfo").lower()
    if "hypervisor" in cpuinfo:
        return True
    return "kvm" in _read("/sys/class/dmi/id/product_name").lower()


_SERVER_CHASSIS = {"23", "25", "28", "29"}


def _server_chassis(value: str) -> str:
    value = value.strip()
    if value in _SERVER_CHASSIS:
        return "server"
    if value:
        return "desktop"
    return ""


def _server_product(value: str) -> str:
    low = value.lower()
    if any(tag in low for tag in ("poweredge", "proliant", "thinksystem", "primergy")):
        return "server"
    return ""


def device_payload() -> dict[str, object]:
    """The device block the relay accepts, with any unavailable field omitted.

    Absent fields are left out entirely rather than sent empty: the relay counts
    only the markers actually supplied. The same goes for the descriptive fields:
    an empty string would be a value, not an absence.

    hypervisor is sent only when it is true. Sending false would be wrong for any
    machine that does not expose the signal -- macOS and Windows, for instance --
    and the relay already treats a missing field as "not observed".
    """
    payload: dict[str, object] = {
        "os_family": platform.system().lower(),
        "arch": platform.machine().lower(),
        "runner_version": __version__,
        "python_version": platform.python_version(),
    }
    if m := machine_id():
        payload["machine_id"] = m
    if d := disk_id():
        payload["disk_uuid"] = d
    if cls := device_class():
        payload["device_class"] = cls
    if hypervisor_present():
        payload["hypervisor"] = True
    return payload
