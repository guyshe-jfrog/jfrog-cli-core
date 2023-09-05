package audit

import (
	"errors"
	"github.com/jfrog/gofrog/version"
	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-cli-core/v2/xray/commands/audit/jas"
	"github.com/jfrog/jfrog-cli-core/v2/xray/commands/audit/jas/applicability"
	"github.com/jfrog/jfrog-cli-core/v2/xray/commands/audit/jas/iac"
	"github.com/jfrog/jfrog-cli-core/v2/xray/commands/audit/jas/sast"
	"github.com/jfrog/jfrog-cli-core/v2/xray/commands/audit/jas/secrets"
	"github.com/jfrog/jfrog-cli-core/v2/xray/utils"
	"github.com/jfrog/jfrog-client-go/utils/io"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

func runJasScannersAndSetResults(scanResults *utils.ExtendedScanResults, directDependencies []string,
	serverDetails *config.ServerDetails, workingDirs []string, progress io.ProgressMgr) (err error) {
	if serverDetails == nil || len(serverDetails.Url) == 0 {
		log.Warn("To include 'Advanced Security' scan as part of the audit output, please run the 'jf c add' command before running this command.")
		return
	}
	scanner, err := jas.NewJasScanner(workingDirs, serverDetails)
	if err != nil {
		return
	}
	defer func() {
		cleanup := scanner.ScannerDirCleanupFunc
		err = errors.Join(err, cleanup())
	}()
	if progress != nil {
		progress.SetHeadlineMsg("Running applicability scanning")
	}
	scanResults.ApplicabilityScanResults, err = applicability.RunApplicabilityScan(scanResults.XrayResults, directDependencies, scanResults.ScannedTechnologies, scanner)
	if err != nil {
		return
	}
	if progress != nil {
		progress.SetHeadlineMsg("Running secrets scanning")
	}
	scanResults.SecretsScanResults, err = secrets.RunSecretsScan(scanner)
	if err != nil {
		return
	}
	if progress != nil {
		progress.SetHeadlineMsg("Running IaC scanning")
	}
	scanResults.IacScanResults, err = iac.RunIacScan(scanner)
	if err != nil {
		return
	}
	if !version.NewVersion(utils.AnalyzerManagerVersion).AtLeast(utils.MinAnalyzerManagerVersionForSast) {
		return
	}
	if progress != nil {
		progress.SetHeadlineMsg("Running SAST scanning")
	}
	scanResults.SastResults, err = sast.RunSastScan(scanner)
	return
}