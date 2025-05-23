// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-present Datadog, Inc.

// Package flare contains helpers and e2e tests of the flare command
package diagnose

import (
	"testing"

	"github.com/DataDog/test-infra-definitions/components/datadog/agentparams"
	"github.com/stretchr/testify/assert"

	"github.com/DataDog/datadog-agent/test/new-e2e/pkg/e2e"
	awshost "github.com/DataDog/datadog-agent/test/new-e2e/pkg/provisioners/aws/host"
	svcmanager "github.com/DataDog/datadog-agent/test/new-e2e/tests/agent-platform/common/svc-manager"
)

type linuxDiagnoseSuite struct {
	baseDiagnoseSuite
}

func TestLinuxDiagnoseSuite(t *testing.T) {
	t.Parallel()
	var suite linuxDiagnoseSuite
	suite.suites = append(suite.suites, commonSuites...)
	e2e.Run(t, &suite, e2e.WithProvisioner(awshost.Provisioner()))
}

func (v *linuxDiagnoseSuite) TestDiagnoseOtherCmdPort() {
	params := agentparams.WithAgentConfig("cmd_port: 4567")
	v.UpdateEnv(awshost.Provisioner(awshost.WithAgentOptions(params)))

	diagnose := getDiagnoseOutput(&v.baseDiagnoseSuite)
	v.AssertOutputNotError(diagnose)
}

func (v *linuxDiagnoseSuite) TestDiagnoseLocalFallback() {
	svcManager := svcmanager.NewSystemctl(v.Env().RemoteHost)
	svcManager.Stop("datadog-agent")

	diagnose := getDiagnoseOutput(&v.baseDiagnoseSuite)
	assert.Contains(v.T(), diagnose, "Running diagnose command locally", "Expected diagnose command to fallback to local diagnosis when the Agent is stopped, but it did not.")
	v.AssertOutputNotError(diagnose)

	svcManager.Start("datadog-agent")
}

func (v *linuxDiagnoseSuite) TestDiagnoseInclude() {
	v.AssertDiagnoseInclude()
	v.AssertDiagnoseJSONInclude()
}

func (v *linuxDiagnoseSuite) TestDiagnoseExclude() {
	v.AssertDiagnoseExclude()
	v.AssertDiagnoseJSONExclude()
}
