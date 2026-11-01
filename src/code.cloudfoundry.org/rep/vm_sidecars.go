package rep

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"code.cloudfoundry.org/bbs/models"
)

const (
	// vmSidecarsEnvVar names the variable that carries the folded sidecars to
	// vm-launch, which reads it from the launcher RunAction's environment.
	vmSidecarsEnvVar = "CF_VM_SIDECARS"

	// vmSidecarRelayEnvVar names the variable that turns a sidecar's process
	// into a log relay. Its value is <index>:<key>, where the index is the
	// sidecar's place in the description counted from 1.
	vmSidecarRelayEnvVar = "CF_VM_SIDECAR_RELAY"

	// vmSidecarsVersion is the version of the description that vm-launch
	// accepts. Version 2 adds the relay key.
	vmSidecarsVersion = 2

	// vmSidecarRelayKeyBytes is the size of the random relay key before it is
	// encoded as hex.
	vmSidecarRelayKeyBytes = 16

	// maxVMSidecars and maxVMSidecarsBytes bound the description. The byte
	// limit keeps it well under the 128 KiB that one environment string may
	// hold.
	maxVMSidecars      = 8
	maxVMSidecarsBytes = 64 * 1024
)

// cfSSHDPath is the SSH daemon that Cloud Controller adds beside the app and
// its sidecars. It stays in the folded action as a Garden process.
const cfSSHDPath = "/tmp/lifecycle/diego-sshd"

// vmSidecarsDescription is the JSON value of CF_VM_SIDECARS.
type vmSidecarsDescription struct {
	Version  int         `json:"version"`
	RelayKey string      `json:"relay_key"`
	Sidecars []vmSidecar `json:"sidecars"`
}

// vmSidecar describes one sidecar. Env holds the variables whose last value
// differs from the app action's, as NAME=value, and Unset names the variables
// that the app action sets and the sidecar action does not.
type vmSidecar struct {
	Args      []string `json:"args"`
	Env       []string `json:"env"`
	Unset     []string `json:"unset"`
	LogSource string   `json:"log_source"`
}

// foldVMSidecars returns a copy of the LRP's app action whose launcher
// RunAction carries the sidecars as the CF_VM_SIDECARS variable, together with
// a copy of the LRP's Sidecars field. A VM backend boots one VM per launcher
// process, so the sidecars cannot run as Garden processes of their own, and
// vm-launch starts them next to the app inside the guest instead.
//
// Each sidecar stays in place as a relay action that keeps its path, user, log
// source, and resource limits, and has no arguments and one variable,
// CF_VM_SIDECAR_RELAY=<index>:<key>. The executor streams the relay's output
// under the sidecar's log source, and vm-launch feeds the relay the sidecar's
// lines. The key is made once for each call and also travels in the
// description, so only the app's launcher and the relays know it. The app's
// launcher gets an empty CF_VM_SIDECAR_RELAY, so a value that the user set in
// the app's environment never turns the app into a relay.
//
// Cloud Controller builds the app action as a codependent action whose first
// launcher RunAction is the app, whose next launcher RunActions are the
// sidecars, and whose other children, such as the SSH daemon, are not launcher
// processes. The fold also takes the sidecars of the LRP's Sidecars field, after
// those. It succeeds only when the launchers sit directly in that codependent
// action, its only other children are runs of the SSH daemon, every sidecar is a single launcher RunAction run as the same user as
// the app, there are at most eight sidecars, and the encoded description is at
// most 64 KiB. Every refusal wraps ErrVMBackendSidecars and names the rule that
// failed. The desired LRP is never changed.
func foldVMSidecars(desiredLRP *models.DesiredLRP) (*models.Action, []*models.Sidecar, error) {
	if desiredLRP.Action == nil {
		return nil, nil, fmt.Errorf("%w: the app has no action", ErrVMBackendSidecars)
	}

	data, err := desiredLRP.Action.Marshal()
	if err != nil {
		return nil, nil, fmt.Errorf("copy the app action: %w", err)
	}
	folded := &models.Action{}
	if err := folded.Unmarshal(data); err != nil {
		return nil, nil, fmt.Errorf("copy the app action: %w", err)
	}

	// The launchers must be the direct children of one codependent action, or
	// the whole action when it is a single RunAction.
	var children []*models.Action
	switch top := models.UnwrapAction(folded).(type) {
	case *models.CodependentAction:
		children = top.Actions
	case *models.RunAction:
		children = []*models.Action{folded}
	}
	var direct []*models.RunAction
	for _, child := range children {
		run, ok := models.UnwrapAction(child).(*models.RunAction)
		switch {
		case ok && run != nil && run.Path == cfLauncherPath:
			direct = append(direct, run)
		case ok && run != nil && run.Path == cfSSHDPath:
		default:
			return nil, nil, fmt.Errorf("%w: the app action holds a process that is neither the launcher nor the SSH daemon", ErrVMBackendSidecars)
		}
	}
	if len(direct) == 0 {
		return nil, nil, fmt.Errorf("%w: the app action holds no launcher process", ErrVMBackendSidecars)
	}
	app := direct[0]

	// Each sidecar is a RunAction with a label for its place, such as "sidecar 2".
	// A sidecar from the Sidecars field also remembers its entry there.
	type labeled struct {
		label string
		run   *models.RunAction
		field *models.Sidecar
	}
	var sidecars []labeled
	for _, run := range direct[1:] {
		sidecars = append(sidecars, labeled{label: fmt.Sprintf("sidecar %d", len(sidecars)+1), run: run})
	}
	for _, sidecar := range desiredLRP.Sidecars {
		label := fmt.Sprintf("sidecar %d", len(sidecars)+1)
		if sidecar == nil || sidecar.Action == nil {
			return nil, nil, fmt.Errorf("%w: %s has no action", ErrVMBackendSidecars, label)
		}
		run, ok := models.UnwrapAction(sidecar.Action).(*models.RunAction)
		if !ok || run == nil {
			return nil, nil, fmt.Errorf("%w: %s is not a single run action", ErrVMBackendSidecars, label)
		}
		if run.Path != cfLauncherPath {
			return nil, nil, fmt.Errorf("%w: %s does not run through the launcher at %s", ErrVMBackendSidecars, label, cfLauncherPath)
		}
		sidecars = append(sidecars, labeled{label: label, run: run, field: sidecar})
	}
	if len(sidecars) > maxVMSidecars {
		return nil, nil, fmt.Errorf("%w: the app has %d sidecars, and at most %d are allowed", ErrVMBackendSidecars, len(sidecars), maxVMSidecars)
	}

	keyBytes := make([]byte, vmSidecarRelayKeyBytes)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, nil, fmt.Errorf("make the sidecar relay key: %w", err)
	}
	relayKey := hex.EncodeToString(keyBytes)

	description := vmSidecarsDescription{Version: vmSidecarsVersion, RelayKey: relayKey, Sidecars: make([]vmSidecar, 0, len(sidecars))}
	for _, sidecar := range sidecars {
		run := sidecar.run
		if run.User != app.User {
			return nil, nil, fmt.Errorf("%w: %s runs as user %q, and the app runs as %q", ErrVMBackendSidecars, sidecar.label, run.User, app.User)
		}

		env, unset := vmSidecarEnvDiff(app.Env, run.Env)
		args := run.Args
		if args == nil {
			args = []string{}
		}
		description.Sidecars = append(description.Sidecars, vmSidecar{
			Args:      args,
			Env:       env,
			Unset:     unset,
			LogSource: run.LogSource,
		})
	}

	encoded, err := json.Marshal(description)
	if err != nil {
		return nil, nil, fmt.Errorf("encode the sidecars: %w", err)
	}
	if len(encoded) > maxVMSidecarsBytes {
		return nil, nil, fmt.Errorf("%w: the sidecar description is %d bytes, and at most %d are allowed", ErrVMBackendSidecars, len(encoded), maxVMSidecarsBytes)
	}

	// Turn each sidecar into its relay action, since vm-launch starts the
	// sidecar itself. The sidecars in the action are rewritten in place in the
	// copy, and the ones in the Sidecars field are copied into new entries.
	var fieldSidecars []*models.Sidecar
	for i, sidecar := range sidecars {
		relay := &models.RunAction{
			Path:           sidecar.run.Path,
			User:           sidecar.run.User,
			LogSource:      sidecar.run.LogSource,
			ResourceLimits: sidecar.run.ResourceLimits,
			Env:            []*models.EnvironmentVariable{{Name: vmSidecarRelayEnvVar, Value: fmt.Sprintf("%d:%s", i+1, relayKey)}},
		}
		if sidecar.field == nil {
			*sidecar.run = *relay
			continue
		}
		copied := *sidecar.field
		copied.Action = models.WrapAction(relay)
		fieldSidecars = append(fieldSidecars, &copied)
	}

	// Give the app's launcher the description and an empty relay variable.
	app.Env = append(app.Env,
		&models.EnvironmentVariable{Name: vmSidecarsEnvVar, Value: string(encoded)},
		&models.EnvironmentVariable{Name: vmSidecarRelayEnvVar, Value: ""},
	)
	return folded, fieldSidecars, nil
}

// vmSidecarEnvDiff compares the last value of each variable. It returns, in
// the order the sidecar lists its names, every NAME=value whose value differs
// from the app's or is missing there, and, in the order the app lists its
// names, every name that only the app sets.
func vmSidecarEnvDiff(appEnv, sidecarEnv []*models.EnvironmentVariable) (env, unset []string) {
	env, unset = []string{}, []string{}

	seen := map[string]bool{}
	for _, v := range sidecarEnv {
		if v == nil || seen[v.Name] {
			continue
		}
		seen[v.Name] = true
		value, _ := lastEnvValue(sidecarEnv, v.Name)
		if appValue, ok := lastEnvValue(appEnv, v.Name); !ok || appValue != value {
			env = append(env, v.Name+"="+value)
		}
	}

	listed := map[string]bool{}
	for _, v := range appEnv {
		if v == nil || listed[v.Name] || seen[v.Name] {
			continue
		}
		listed[v.Name] = true
		unset = append(unset, v.Name)
	}
	return env, unset
}
