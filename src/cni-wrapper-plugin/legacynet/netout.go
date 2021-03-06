package legacynet

import (
	"errors"
	"fmt"
	"lib/rules"
	"net"

	multierror "github.com/hashicorp/go-multierror"

	"code.cloudfoundry.org/garden"
)

const prefixInput = "input"
const prefixNetOut = "netout"
const prefixOverlay = "overlay"
const suffixNetOutLog = "log"

//go:generate counterfeiter -o ../fakes/net_out_rule_converter.go --fake-name NetOutRuleConverter . netOutRuleConverter
type netOutRuleConverter interface {
	Convert(rule garden.NetOutRule, containerIP, logChainName string, logging bool) []rules.IPTablesRule
	BulkConvert(rules []garden.NetOutRule, containerIP, logChainName string, logging bool) []rules.IPTablesRule
}

type NetOut struct {
	ChainNamer chainNamer
	IPTables   rules.IPTablesAdapter
	Converter  netOutRuleConverter
	ASGLogging bool
	C2CLogging bool
	IngressTag string
	VTEPName   string
}

type fullRule struct {
	Table       string
	ParentChain string
	Chain       string
	Rules       []rules.IPTablesRule
}

func (m *NetOut) Initialize(containerHandle string, containerIP net.IP, dnsServers []string) error {
	if containerHandle == "" {
		return errors.New("invalid handle")
	}

	inputChain := m.ChainNamer.Prefix(prefixInput, containerHandle)
	forwardChain := m.ChainNamer.Prefix(prefixNetOut, containerHandle)
	overlayChain := m.ChainNamer.Prefix(prefixOverlay, containerHandle)
	logChain, err := m.ChainNamer.Postfix(forwardChain, suffixNetOutLog)
	if err != nil {
		return fmt.Errorf("getting chain name: %s", err)
	}

	args := []fullRule{
		{
			Table:       "filter",
			ParentChain: "INPUT",
			Chain:       inputChain,
			Rules: []rules.IPTablesRule{
				rules.NewInputRelatedEstablishedRule(containerIP.String()),
				rules.NewInputDefaultRejectRule(containerIP.String()),
			},
		},
		{
			Table:       "filter",
			ParentChain: "FORWARD",
			Chain:       forwardChain,
			Rules: []rules.IPTablesRule{
				rules.NewNetOutRelatedEstablishedRule(containerIP.String()),
				rules.NewNetOutDefaultRejectRule(containerIP.String(), m.VTEPName),
			},
		},
		{
			Table:       "filter",
			ParentChain: "FORWARD",
			Chain:       overlayChain,
			Rules: []rules.IPTablesRule{
				rules.NewOverlayAllowEgress(m.VTEPName, containerIP.String()),
				rules.NewOverlayRelatedEstablishedRule(containerIP.String()),
				rules.NewOverlayTagAcceptRule(containerIP.String(), m.IngressTag),
				rules.NewOverlayDefaultRejectRule(containerIP.String()),
			},
		},
		{
			Table: "filter",
			Chain: logChain,
			Rules: []rules.IPTablesRule{
				rules.NewNetOutDefaultLogRule(containerHandle),
				rules.NewAcceptRule(),
			},
		},
	}

	if m.ASGLogging {
		args[1].Rules = []rules.IPTablesRule{
			rules.NewNetOutRelatedEstablishedRule(containerIP.String()),
			rules.NewNetOutDefaultRejectLogRule(containerHandle, containerIP.String(), m.VTEPName),
			rules.NewNetOutDefaultRejectRule(containerIP.String(), m.VTEPName),
		}
	}

	if m.C2CLogging {
		args[2].Rules = []rules.IPTablesRule{
			rules.NewOverlayAllowEgress(m.VTEPName, containerIP.String()),
			rules.NewOverlayRelatedEstablishedRule(containerIP.String()),
			rules.NewOverlayTagAcceptRule(containerIP.String(), m.IngressTag),
			rules.NewOverlayDefaultRejectLogRule(containerHandle, containerIP.String()),
			rules.NewOverlayDefaultRejectRule(containerIP.String()),
		}
	}

	if len(dnsServers) > 0 {
		args[0].Rules = []rules.IPTablesRule{
			rules.NewInputRelatedEstablishedRule(containerIP.String()),
		}
		for _, dnsServer := range dnsServers {
			args[0].Rules = append(args[0].Rules, rules.NewInputAllowRule(containerIP.String(), "tcp", dnsServer, 53))
			args[0].Rules = append(args[0].Rules, rules.NewInputAllowRule(containerIP.String(), "udp", dnsServer, 53))
		}
		args[0].Rules = append(args[0].Rules, rules.NewInputDefaultRejectRule(containerIP.String()))
	}

	err = initChains(m.IPTables, args)
	if err != nil {
		return err
	}

	return applyRules(m.IPTables, args)
}

func (m *NetOut) Cleanup(containerHandle string) error {
	overlayChain := m.ChainNamer.Prefix(prefixOverlay, containerHandle)
	forwardChain := m.ChainNamer.Prefix(prefixNetOut, containerHandle)
	inputChain := m.ChainNamer.Prefix(prefixInput, containerHandle)
	logChain, err := m.ChainNamer.Postfix(forwardChain, suffixNetOutLog)
	if err != nil {
		return fmt.Errorf("getting chain name: %s", err)
	}

	var result error
	if err := cleanupChain("filter", "FORWARD", overlayChain, m.IPTables); err != nil {
		result = multierror.Append(result, err)
	}
	if err := cleanupChain("filter", "FORWARD", forwardChain, m.IPTables); err != nil {
		result = multierror.Append(result, err)
	}
	if err := cleanupChain("filter", "INPUT", inputChain, m.IPTables); err != nil {
		result = multierror.Append(result, err)
	}
	if err := cleanupChain("filter", "", logChain, m.IPTables); err != nil {
		result = multierror.Append(result, err)
	}

	return result
}

func (m *NetOut) InsertRule(containerHandle string, rule garden.NetOutRule, containerIP string) error {
	chain := m.ChainNamer.Prefix(prefixNetOut, containerHandle)
	logChain, err := m.ChainNamer.Postfix(chain, suffixNetOutLog)
	if err != nil {
		return fmt.Errorf("getting chain name: %s", err)
	}

	ruleSpec := m.Converter.Convert(rule, containerIP, logChain, m.ASGLogging)
	err = m.IPTables.BulkInsert("filter", chain, 1, ruleSpec...)
	if err != nil {
		return fmt.Errorf("inserting net-out rule: %s", err)
	}

	return nil
}

func (m *NetOut) BulkInsertRules(containerHandle string, netOutRules []garden.NetOutRule, containerIP string) error {
	chain := m.ChainNamer.Prefix(prefixNetOut, containerHandle)
	logChain, err := m.ChainNamer.Postfix(chain, suffixNetOutLog)
	if err != nil {
		return fmt.Errorf("getting chain name: %s", err)
	}

	ruleSpec := m.Converter.BulkConvert(netOutRules, containerIP, logChain, m.ASGLogging)
	err = m.IPTables.BulkInsert("filter", chain, 1, ruleSpec...)
	if err != nil {
		return fmt.Errorf("bulk inserting net-out rules: %s", err)
	}

	return nil
}
