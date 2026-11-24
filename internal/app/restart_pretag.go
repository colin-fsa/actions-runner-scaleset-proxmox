package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/jeffresc/actions-runner-scaleset-proxmox/internal/config"
)

func pretagProfile(ctx context.Context, cli recoveryReader, cfg *config.Config, set config.ScaleSetEntry, kind string, vmid int, tasks map[string]restartTask) (string, string, error) {
	// Never infer destination from a clone UPID: its ID is the source.
	type expected struct{ profile, line string }
	bySource := map[string][]expected{}
	logSubject, taskKind := "VM", "qmclone"
	if kind == "lxc" {
		logSubject, taskKind = "container", "vzclone"
	}
	for _, p := range set.Profiles {
		if p.GuestTypeOrDefault() != kind {
			continue
		}
		stable := p.TemplateVMID
		if stable == 0 {
			stable = cfg.Proxmox.TemplateVMID
		}
		for _, source := range []int{stable, p.CanaryTemplateVMID} {
			if source <= 0 {
				continue
			}
			id := strconv.Itoa(source)
			candidate := expected{p.Name, fmt.Sprintf("creating a clone of %s %d with ID %d", logSubject, source, vmid)}
			known := false
			for _, previous := range bySource[id] {
				known = known || previous == candidate
			}
			if !known {
				bySource[id] = append(bySource[id], candidate)
			}
		}
	}
	base := "/nodes/" + url.PathEscape(cfg.Nodes.SingleNode) + "/tasks/"
	profile, receipt, checked := "", "", 0
	for _, task := range tasks {
		candidates := bySource[task.ID]
		if task.Type != taskKind || task.Status != "OK" || len(candidates) == 0 {
			continue
		}
		owner, err := restartTaskOwner(task, cfg.Nodes.SingleNode)
		if err != nil {
			return "", "", err
		}
		if owner != cfg.Proxmox.Auth.TokenID {
			continue
		}
		checked++
		if checked > 16 {
			return "", "", errors.New("pre-tag clone log search is capped")
		}
		path := base + url.PathEscape(task.UPID)
		var lines []struct {
			Number int    `json:"n"`
			Text   string `json:"t"`
		}
		if err := cli.Get(ctx, path+"/log?start=0&limit=8", &lines); err != nil || lines == nil || len(lines) > 8 {
			return "", "", errors.New("own archived clone log unavailable or incomplete")
		}
		for _, line := range lines {
			for _, candidate := range candidates {
				// These are the actual PVE worker prologues. No prefix,
				// substring, extra text or alternate numeric spelling.
				if line.Number != 1 || line.Text != candidate.line {
					continue
				}
				if receipt != "" || len(candidates) != 1 {
					return "", "", errors.New("ambiguous clone destination or template profile")
				}
				var terminal struct {
					UPID, Node, Type, ID, Status, ExitStatus string
					Start                                    int64 `json:"starttime"`
				}
				if err := cli.Get(ctx, path+"/status", &terminal); err != nil || terminal.UPID != task.UPID || terminal.Node != task.Node ||
					terminal.Type != task.Type || terminal.ID != task.ID || terminal.Start != task.Start ||
					terminal.Status != "stopped" || terminal.ExitStatus != "OK" {
					return "", "", errors.New("pre-tag clone terminal success unproven")
				}
				profile, receipt = candidate.profile, task.UPID
			}
		}
	}
	if receipt == "" {
		return "", "", errors.New("no exact owned archived clone targets this pre-tag guest")
	}
	return profile, receipt, nil
}

func hasManagedGuestTags(wire string) bool {
	for _, tag := range strings.Split(wire, ";") {
		t := strings.TrimSpace(tag)
		if t == "gh-scaleset" || strings.HasPrefix(t, "gh-scaleset-") {
			return true
		}
	}
	return false
}
