package service

import (
	"bytes"
	"encoding/xml"
	"fmt"
)

// windowsTaskName is the Task Scheduler task that runs the agent.
const windowsTaskName = "QuantifaiSync"

// taskTemplate is the Task Scheduler definition for the Windows agent: a
// per-user logon task, the counterpart of the macOS LaunchAgent and the
// systemd user unit. It runs as the installing user (InteractiveToken,
// LeastPrivilege), so the agent sees that user's home folder, config and
// Credential Manager key, which a LocalSystem service cannot. The trigger
// names the same user; a bare logon trigger fires for anyone who logs on.
//
// Settings Task Scheduler gets wrong for a long-running agent are set
// explicitly: no execution time limit (the default stops a task after 72
// hours), and start and keep running on battery. StopExisting makes a
// /run (as install does) replace a running instance instead of being
// dropped, without reading the task's localized status.
//
// RestartOnFailure is left out: it did not restart a run that exited 1
// (measured on windows-latest). Crash restart comes from `run --supervise`.
//
// Placeholders, in order: user (trigger), user (principal), command,
// arguments. All are XML-escaped by GenerateTaskXML.
const taskTemplate = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Quantifai Sync Agent</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>%s</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>StopExisting</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
      <Arguments>%s</Arguments>
    </Exec>
  </Actions>
</Task>
`

// taskArguments starts the agent with its console window hidden, under a
// supervisor that restarts it after a crash.
const taskArguments = "run --no-console --supervise"

// GenerateTaskXML returns the task definition for user running binPath
// with args.
func GenerateTaskXML(user, binPath, args string) string {
	return fmt.Sprintf(taskTemplate, xmlEscape(user), xmlEscape(user), xmlEscape(binPath), xmlEscape(args))
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}
