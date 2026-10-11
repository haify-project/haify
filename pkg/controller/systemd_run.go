package controller

// collectFlag expands, in the node's shell, to systemd-run's --collect where
// that systemd has it (236 and later) and to nothing where it does not: the
// systemd 219 of RHEL/CentOS 7, which XCP-ng 8 runs in dom0, refuses the
// unknown option and the whole command with it. --collect only makes systemd
// forget a failed transient unit, so leaving it out costs nothing but a unit
// to reset-failed.
const collectFlag = "$(systemd-run --help 2>/dev/null | grep -q -e --collect && echo --collect)"
