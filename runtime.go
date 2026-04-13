package monolith

// This file previously contained Run() which booted the VM in the foreground.
// That has been replaced by RunDaemon() in vmd.go, which boots the VM as a
// background daemon with a PTY for the serial console and a Unix socket for
// client attachment and control commands.
//
// The old Run() function wired os.Stdin/os.Stdout directly to the serial port,
// making it impossible to detach from the VM without killing it. The daemon
// model allows the VM to continue running after the user disconnects.
