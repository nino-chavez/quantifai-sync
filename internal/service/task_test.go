package service

import (
	"strings"
	"testing"
)

// Expected values are literal: they are what Task Scheduler must store.
func TestTaskXMLSettings(t *testing.T) {
	x := GenerateTaskXML(`DESKTOP-1\ana`, `C:\Users\ana\bin\quantifai-sync.exe`, taskArguments)
	for _, want := range []string{
		`<LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>DESKTOP-1\ana</UserId>`,
		`<UserId>DESKTOP-1\ana</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>`,
		`<Command>C:\Users\ana\bin\quantifai-sync.exe</Command>`,
		`<Arguments>run --no-console</Arguments>`,
		`<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>`,
		`<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>`,
		`<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>`,
		`<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>`,
	} {
		if !strings.Contains(x, want) {
			t.Errorf("task XML missing:\n%s\n--- got:\n%s", want, x)
		}
	}
}

func TestTaskXMLEscapesValues(t *testing.T) {
	x := GenerateTaskXML(`CORP\o'brien&co`, `C:\Program Files\<odd>\q.exe`, taskArguments)
	for _, want := range []string{
		`<UserId>CORP\o&#39;brien&amp;co</UserId>`,
		`<Command>C:\Program Files\&lt;odd&gt;\q.exe</Command>`,
	} {
		if !strings.Contains(x, want) {
			t.Errorf("task XML missing escaped %s", want)
		}
	}
}
