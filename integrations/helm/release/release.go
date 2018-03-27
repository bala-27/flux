package release

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	//	"k8s.io/client-go/kubernetes"

	yaml "gopkg.in/yaml.v2"
	k8shelm "k8s.io/helm/pkg/helm"
	hapi_release "k8s.io/helm/pkg/proto/hapi/release"

	"github.com/go-kit/kit/log"
	ifv1 "github.com/weaveworks/flux/apis/helm.integrations.flux.weave.works/v1alpha"
	helmgit "github.com/weaveworks/flux/integrations/helm/git"
)

var (
	ErrChartGitPathMissing = "Chart deploy configuration (%s) has empty Chart git path"
)

// ReleaseType determines whether we are making a new Chart release or updating an existing one
type ReleaseType string

// Release contains clients needed to provide functionality related to helm releases
type Release struct {
	logger     log.Logger
	HelmClient *k8shelm.Client
	Repo       repo
	sync.RWMutex
}

type repo struct {
	ConfigSync   *helmgit.Checkout
	ChartsSync   *helmgit.Checkout
	ReleasesSync *helmgit.Checkout
}

type DeployInfo struct {
	Name     string
	Deployed int64
}

type InstallOptions struct {
	DryRun    bool
	ReuseName bool
}

// New creates a new Release instance
func New(logger log.Logger, helmClient *k8shelm.Client, configCheckout *helmgit.Checkout, chartsSync *helmgit.Checkout, releasesSync *helmgit.Checkout) *Release {
	repo := repo{
		ConfigSync:   configCheckout,
		ChartsSync:   chartsSync,
		ReleasesSync: releasesSync,
	}
	r := &Release{
		logger:     logger,
		HelmClient: helmClient,
		Repo:       repo,
	}
	return r
}

// GetReleaseName either retrieves the release name from the Custom Resource or constructs a new one
//  in the form : $Namespace-$CustomResourceName
func GetReleaseName(fhr ifv1.FluxHelmRelease) string {
	namespace := fhr.Namespace
	if namespace == "" {
		namespace = "default"
	}
	releaseName := fhr.Spec.ReleaseName
	if releaseName == "" {
		releaseName = fmt.Sprintf("%s-%s", namespace, fhr.Name)
	}

	return releaseName
}

// Exists ... detects if a particular Chart release exists
// 		release name must match regex ^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])+$
// Get ... detects if a particular Chart release exists
// 		release name must match regex ^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])+$
func (r *Release) Exists(name string) (bool, error) {
	r.Lock()
	rls, err := r.HelmClient.ReleaseContent(name)
	r.Unlock()

	if err != nil {
		//r.logger.Log("debug", fmt.Sprintf("Getting release (%s): %v", name, err))
		return false, err
	}
	/*
		"UNKNOWN":          0,
		"DEPLOYED":         1,
		"DELETED":          2,
		"SUPERSEDED":       3,
		"FAILED":           4,
		"DELETING":         5,
		"PENDING_INSTALL":  6,
		"PENDING_UPGRADE":  7,
		"PENDING_ROLLBACK": 8,
	*/
	rst := rls.Release.Info.Status.GetCode()
	r.logger.Log("info", fmt.Sprintf("Found release [%s] with status %s", name, rst.String()))

	if rst == 1 || rst == 4 {
		return true, nil
	}
	return true, fmt.Errorf("Release [%s] exists with status: %s", name, rst.String())
}

func (r *Release) canDelete(name string) (bool, error) {
	r.Lock()
	rls, err := r.HelmClient.ReleaseStatus(name)
	r.Unlock()

	if err != nil {
		r.logger.Log("error", fmt.Sprintf("Error finding status for release (%s): %#v", name, err))
		return false, err
	}
	/*
		"UNKNOWN":          0,
		"DEPLOYED":         1,
		"DELETED":          2,
		"SUPERSEDED":       3,
		"FAILED":           4,
		"DELETING":         5,
		"PENDING_INSTALL":  6,
		"PENDING_UPGRADE":  7,
		"PENDING_ROLLBACK": 8,
	*/
	status := rls.GetInfo().GetStatus()
	r.logger.Log("info", fmt.Sprintf("Release [%s] status: %s", name, status.Code.String()))
	switch status.Code {
	case 1, 4:
		r.logger.Log("info", fmt.Sprintf("Deleting release (%s)", name))
		return true, nil
	case 2:
		r.logger.Log("info", fmt.Sprintf("Release (%s) already deleted", name))
		return false, nil
	default:
		r.logger.Log("info", fmt.Sprintf("Release (%s) with status %s cannot be deleted", name, status.Code.String()))
		return false, fmt.Errorf("Release (%s) with status %s cannot be deleted", name, status.Code.String())
	}
}

// Install ... performs Chart release. Depending on the release type, this is either a new release,
// or an upgrade of an existing one
func (r *Release) Install(checkout *helmgit.Checkout, releaseName string, fhr ifv1.FluxHelmRelease, releaseType ReleaseType, opts InstallOptions) (hapi_release.Release, error) {
	r.logger.Log("info", fmt.Sprintf("releaseName= %s, releaseType=%s", releaseName, releaseType))

	chartPath := fhr.Spec.ChartGitPath
	if chartPath == "" {
		r.logger.Log("error", fmt.Sprintf(ErrChartGitPathMissing, fhr.GetName()))
		return hapi_release.Release{}, fmt.Errorf(ErrChartGitPathMissing, fhr.GetName())
	}

	namespace := fhr.GetNamespace()
	if namespace == "" {
		namespace = "default"
	}

	ctx, cancel := context.WithTimeout(context.Background(), helmgit.DefaultCloneTimeout)
	err := checkout.Pull(ctx)
	cancel()
	if err != nil {
		errm := fmt.Errorf("Failure to do git pull: %#v", err)
		r.logger.Log("error", errm.Error())
		return hapi_release.Release{}, errm
	}

	chartDir := filepath.Join(checkout.Dir, checkout.Config.Path, chartPath)

	rawVals, err := collectValues(fhr.Spec.Values)
	if err != nil {
		r.logger.Log("error", fmt.Sprintf("Problem with supplied customizations for Chart release [%s]: %#v", releaseName, err))
		return hapi_release.Release{}, err
	}

	// INSTALLATION ----------------------------------------------------------------------
	switch releaseType {
	case "CREATE":
		r.Lock()
		res, err := r.HelmClient.InstallRelease(
			chartDir,
			namespace,
			k8shelm.ValueOverrides(rawVals),
			k8shelm.ReleaseName(releaseName),
			k8shelm.InstallDryRun(opts.DryRun),
			k8shelm.InstallReuseName(opts.ReuseName),
			/*
				helm.InstallReuseName(i.replace),
				helm.InstallDisableHooks(i.disableHooks),
				helm.InstallTimeout(i.timeout),
				helm.InstallWait(i.wait)
			*/
		)
		r.Unlock()

		if err != nil {
			r.logger.Log("error", fmt.Sprintf("Chart release failed: %s: %#v", releaseName, err))
			return hapi_release.Release{}, err
		}
		return *res.Release, nil
	case "UPDATE":
		r.Lock()
		res, err := r.HelmClient.UpdateRelease(
			releaseName,
			chartDir,
			k8shelm.UpdateValueOverrides(rawVals),
			k8shelm.UpgradeDryRun(opts.DryRun),
			/*
				helm.UpgradeRecreate(u.recreate),
				helm.UpgradeForce(u.force),
				helm.UpgradeDisableHooks(u.disableHooks),
				helm.UpgradeTimeout(u.timeout),
				helm.ResetValues(u.resetValues),
				helm.ReuseValues(u.reuseValues),
				helm.UpgradeWait(u.wait))
			*/
		)
		r.Unlock()

		if err != nil {
			r.logger.Log("error", fmt.Sprintf("Chart upgrade release failed: %s: %#v", releaseName, err))
			return hapi_release.Release{}, err
		}
		return *res.Release, nil
	default:
		err = fmt.Errorf("Valid ReleaseType options: CREATE, UPDATE. Provided: %s", releaseType)
		r.logger.Log("error", err.Error())
		return hapi_release.Release{}, err
	}
}

// Delete ... deletes Chart release
func (r *Release) Delete(name string) error {
	ok, err := r.canDelete(name)
	if !ok {
		if err != nil {
			return err
		}
		return nil
	}

	r.Lock()
	_, err = r.HelmClient.DeleteRelease(name)
	r.Unlock()
	if err != nil {
		r.logger.Log("error", fmt.Sprintf("Release deletion error: %#v", err))
		return err
	}
	r.logger.Log("info", fmt.Sprintf("Release deleted: [%s]", name))
	return nil
}

// GetCurrentWithDate provides Chart releases (stored in tiller ConfigMaps)
//		output:
//						map[namespace][release name] = time.Unix() [int64]
func (r *Release) GetCurrentWithDate() (map[string][]DeployInfo, error) {
	response, err := r.HelmClient.ListReleases()
	if err != nil {
		return nil, r.logger.Log("error", err)
	}
	r.logger.Log("info", fmt.Sprintf("Number of Chart releases: %d\n", response.GetCount()))

	relsM := make(map[string][]DeployInfo)
	var nameTime []DeployInfo

	fmt.Println("GETTING CHART RELEASES")
	for _, r := range response.GetReleases() {
		ns := r.Namespace
		nameTime = relsM[ns]
		secs := r.Info.GetLastDeployed().Seconds

		nameTime = append(nameTime, DeployInfo{Name: r.Name, Deployed: secs})
		//fmt.Printf("\n----------\n\t[[%s]] %d ==> %s ... %d\n", ns, i, r.Name, secs)
		//fmt.Printf("\tcurrent number of nameTime elems = %d\n----------\n", len(nameTime))

		relsM[ns] = nameTime
	}
	return relsM, nil
}

func collectValues(params []ifv1.HelmChartParam) ([]byte, error) {
	base := map[string]interface{}{}
	if params == nil || len(params) == 0 {
		return yaml.Marshal(base)
	}

	for _, p := range params {
		k := strings.TrimSpace(p.Name)
		k = strings.Trim(k, "\n")
		if k == "" {
			continue
		}
		v := strings.TrimSpace(p.Value)
		v = strings.Trim(v, "\n")
		base[k] = v
	}

	return yaml.Marshal(base)
}
