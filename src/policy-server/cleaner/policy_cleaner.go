package cleaner

import (
	"context"
	"fmt"
	"policy-server/models"
	"time"

	"code.cloudfoundry.org/lager"
)

//go:generate counterfeiter -o fakes/uua_client.go --fake-name UAAClient . uaaClient
type uaaClient interface {
	GetToken() (string, error)
}

//go:generate counterfeiter -o fakes/cc_client.go --fake-name CCClient . ccClient
type ccClient interface {
	GetLiveAppGUIDs(token string, appGUIDs []string) (map[string]struct{}, error)
}

//go:generate counterfeiter -o fakes/store.go --fake-name Store . store
type store interface {
	All() ([]models.Policy, error)
	Delete([]models.Policy) error
}

//go:generate counterfeiter -o fakes/contextAdapter.go --fake-name ContextAdapter . contextAdapter
type contextAdapter interface {
	WithTimeout(context.Context, time.Duration) (context.Context, context.CancelFunc)
}

type PolicyCleaner struct {
	Logger                lager.Logger
	Store                 store
	UAAClient             uaaClient
	CCClient              ccClient
	CCAppRequestChunkSize int
	RequestTimeout        time.Duration
}

func (p *PolicyCleaner) DeleteStalePolicies() ([]models.Policy, error) {
	policies, err := p.Store.All()
	if err != nil {
		p.Logger.Error("store-list-policies-failed", err)
		return nil, fmt.Errorf("database read failed: %s", err)
	}
	token, err := p.UAAClient.GetToken()
	if err != nil {
		p.Logger.Error("get-uaa-token-failed", err)
		return nil, fmt.Errorf("get UAA token failed: %s", err)
	}

	stalePolicies := []models.Policy{}

	appGUIDs := policyAppGUIDs(policies)
	appGUIDchunks := getChunks(appGUIDs, p.CCAppRequestChunkSize)

	for _, appGUIDchunk := range appGUIDchunks {
		liveAppGUIDs, err := p.CCClient.GetLiveAppGUIDs(token, appGUIDchunk)
		if err != nil {
			p.Logger.Error("cc-get-app-guids-failed", err)
			return nil, fmt.Errorf("get app guids from Cloud-Controller failed: %s", err)
		}

		staleAppGUIDs := getStaleAppGUIDs(liveAppGUIDs, appGUIDchunk)
		toDelete := getStalePolicies(policies, staleAppGUIDs)
		stalePolicies = append(stalePolicies, toDelete...)

		p.Logger.Info("deleting stale policies:", lager.Data{
			"total_policies": len(stalePolicies),
			"stale_policies": stalePolicies,
		})
		err = p.Store.Delete(toDelete)
		if err != nil {
			p.Logger.Error("store-delete-policies-failed", err)
			return nil, fmt.Errorf("database write failed: %s", err)
		}
	}

	return stalePolicies, nil
}

func (p *PolicyCleaner) DeleteStalePoliciesWrapper() error {
	_, err := p.DeleteStalePolicies()
	return err
}

func getStaleAppGUIDs(liveAppGUIDs map[string]struct{}, appGUIDs []string) map[string]struct{} {
	staleAppGUIDs := make(map[string]struct{})
	for _, guid := range appGUIDs {
		if _, ok := liveAppGUIDs[guid]; !ok {
			staleAppGUIDs[guid] = struct{}{}
		}
	}
	return staleAppGUIDs
}

func getStalePolicies(policyList []models.Policy, staleAppGUIDs map[string]struct{}) []models.Policy {
	stalePolicies := []models.Policy{}
	for _, p := range policyList {
		_, foundSrc := staleAppGUIDs[p.Source.ID]
		_, foundDst := staleAppGUIDs[p.Destination.ID]
		if foundSrc || foundDst {
			stalePolicies = append(stalePolicies, p)
		}
	}
	return stalePolicies
}

func policyAppGUIDs(policyList []models.Policy) []string {
	appGUIDset := make(map[string]struct{})
	for _, p := range policyList {
		appGUIDset[p.Source.ID] = struct{}{}
		appGUIDset[p.Destination.ID] = struct{}{}
	}
	var appGUIDs []string
	for guid, _ := range appGUIDset {
		appGUIDs = append(appGUIDs, guid)
	}
	return appGUIDs
}

func getChunks(appGuids []string, chunkSize int) [][]string {
	if chunkSize < 1 {
		chunkSize = 100
	}
	var chunks [][]string

	for i := 0; i < len(appGuids); i += chunkSize {
		last := i + chunkSize
		if last > len(appGuids) {
			last = len(appGuids)
		}
		chunks = append(chunks, appGuids[i:last])
	}
	return chunks
}
