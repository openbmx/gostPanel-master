package service

import (
	stderrors "errors"

	"gost-panel/internal/dto"
	"gost-panel/internal/errors"
	"gost-panel/internal/model"

	"gorm.io/gorm"
)

var supportedTunnelProtocols = map[string]struct{}{
	"tcp":  {},
	"udp":  {},
	"tls":  {},
	"mtls": {},
	"ws":   {},
	"mws":  {},
	"wss":  {},
	"mwss": {},
	"h2":   {},
	"grpc": {},
	"quic": {},
	"kcp":  {},
	"ssh":  {},
}

func (s *TunnelService) normalizeCreateTunnelHops(req *dto.CreateTunnelReq) ([]model.TunnelHop, error) {
	if len(req.Hops) > 0 {
		return s.normalizeTunnelHops(req.EntryNodeID, req.Hops)
	}
	if req.ExitNodeID == 0 {
		return nil, errors.ErrExitNodeNotFound
	}
	protocol := req.Protocol
	if protocol == "" {
		protocol = "ws"
	}
	relayPort := req.RelayPort
	if relayPort == 0 {
		relayPort = 8443
	}
	return s.normalizeTunnelHops(req.EntryNodeID, []dto.TunnelHopReq{{
		NodeID:    req.ExitNodeID,
		Protocol:  protocol,
		RelayPort: relayPort,
	}})
}

func (s *TunnelService) normalizeUpdateTunnelHops(entryNodeID uint, req *dto.UpdateTunnelReq, fallback []model.TunnelHop) ([]model.TunnelHop, error) {
	if len(req.Hops) > 0 {
		return s.normalizeTunnelHops(entryNodeID, req.Hops)
	}
	if len(fallback) == 0 {
		return nil, errors.ErrBadRequest
	}
	hopReqs := make([]dto.TunnelHopReq, 0, len(fallback))
	for _, hop := range fallback {
		protocol := req.Protocol
		if protocol == "" {
			protocol = hop.Protocol
		}
		relayPort := req.RelayPort
		if relayPort == 0 {
			relayPort = hop.RelayPort
		}
		hopReqs = append(hopReqs, dto.TunnelHopReq{
			NodeID:    hop.NodeID,
			Protocol:  protocol,
			RelayPort: relayPort,
		})
	}
	return s.normalizeTunnelHops(entryNodeID, hopReqs)
}

func (s *TunnelService) normalizeTunnelHops(entryNodeID uint, reqHops []dto.TunnelHopReq) ([]model.TunnelHop, error) {
	if len(reqHops) == 0 {
		return nil, errors.ErrBadRequest
	}

	seen := map[uint]struct{}{entryNodeID: {}}
	hops := make([]model.TunnelHop, 0, len(reqHops))
	for _, reqHop := range reqHops {
		if reqHop.NodeID == 0 || reqHop.RelayPort < 1 || reqHop.RelayPort > 65535 {
			return nil, errors.ErrBadRequest
		}
		if _, ok := supportedTunnelProtocols[reqHop.Protocol]; !ok {
			return nil, errors.ErrBadRequest
		}
		if _, ok := seen[reqHop.NodeID]; ok {
			return nil, errors.ErrBadRequest
		}
		if _, err := s.nodeRepo.FindByID(reqHop.NodeID); err != nil {
			if stderrors.Is(err, gorm.ErrRecordNotFound) {
				return nil, errors.ErrExitNodeNotFound
			}
			return nil, err
		}
		seen[reqHop.NodeID] = struct{}{}
		hops = append(hops, model.TunnelHop{
			NodeID:    reqHop.NodeID,
			Protocol:  reqHop.Protocol,
			RelayPort: reqHop.RelayPort,
		})
	}
	return hops, nil
}

func mirrorTunnelLastHop(tunnel *model.GostTunnel, hops []model.TunnelHop) error {
	if len(hops) == 0 {
		return errors.ErrBadRequest
	}
	last := hops[len(hops)-1]
	tunnel.ExitNodeID = last.NodeID
	tunnel.Protocol = last.Protocol
	tunnel.RelayPort = last.RelayPort
	tunnel.Hops = hops
	return nil
}
