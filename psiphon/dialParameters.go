/*
 * Copyright (c) 2018, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package psiphon

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/binary"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/errors"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/fragmentor"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/obfuscator"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/parameters"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/prng"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/protocol"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/regen"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/resolver"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/transforms"
	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common/values"
	"golang.org/x/net/bpf"
)

// DialParameters represents a selected protocol and all the related selected
// protocol attributes, many chosen at random, for a tunnel dial attempt.
//
// DialParameters is used:
// - to configure dialers
// - as a persistent record to store successful dial parameters for replay
// - to report dial stats in notices and Psiphon API calls.
//
// MeekResolvedIPAddress is set asynchronously, as it is not known until the
// dial process has begun. The atomic.Value will contain a string, initialized
// to "", and set to the resolved IP address once that part of the dial
// process has completed.
//
// DialParameters is not safe for concurrent use.
type DialParameters struct {
	ServerEntry             *protocol.ServerEntry `json:"-"`
	NetworkID               string                `json:"-"`
	IsReplay                bool                  `json:"-"`
	CandidateNumber         int                   `json:"-"`
	EstablishedTunnelsCount int                   `json:"-"`

	IsExchanged bool

	LastUsedTimestamp       time.Time
	LastUsedConfigStateHash []byte
	LastUsedServerEntryHash []byte

	NetworkLatencyMultiplier float64

	TunnelProtocol string

	DirectDialAddress              string
	DialPortNumber                 string
	UpstreamProxyType              string   `json:"-"`
	UpstreamProxyCustomHeaderNames []string `json:"-"`

	BPFProgramName         string
	BPFProgramInstructions []bpf.RawInstruction

	SelectedSSHClientVersion bool
	SSHClientVersion         string
	SSHKEXSeed               *prng.Seed

	ObfuscatorPaddingSeed                   *prng.Seed
	OSSHObfuscatorSeedTransformerParameters *transforms.ObfuscatorSeedTransformerParameters

	OSSHPrefixSpec        *obfuscator.OSSHPrefixSpec
	OSSHPrefixSplitConfig *obfuscator.OSSHPrefixSplitConfig

	FragmentorSeed *prng.Seed

	FrontingProviderID string

	MeekFrontingDialAddress   string
	MeekFrontingHost          string
	MeekDialAddress           string
	MeekTransformedHostName   bool
	MeekSNIServerName         string
	MeekVerifyServerName      string
	MeekVerifyPins            []string
	MeekHostHeader            string
	MeekObfuscatorPaddingSeed *prng.Seed
	MeekTLSPaddingSize        int
	MeekResolvedIPAddress     atomic.Value `json:"-"`

	TLSOSSHTransformedSNIServerName bool
	TLSOSSHSNIServerName            string
	TLSOSSHObfuscatorPaddingSeed    *prng.Seed

	SelectedUserAgent bool
	UserAgent         string

	SelectedTLSProfile       bool
	TLSProfile               string
	NoDefaultTLSSessionID    bool
	TLSVersion               string
	RandomizedTLSProfileSeed *prng.Seed
	TLSFragmentClientHello   bool

	QUICVersion                              string
	QUICDialSNIAddress                       string
	QUICClientHelloSeed                      *prng.Seed
	ObfuscatedQUICPaddingSeed                *prng.Seed
	ObfuscatedQUICNonceTransformerParameters *transforms.ObfuscatorSeedTransformerParameters
	QUICDisablePathMTUDiscovery              bool

	ConjureCachedRegistrationTTL        time.Duration
	ConjureAPIRegistration              bool
	ConjureAPIRegistrarBidirectionalURL string
	ConjureAPIRegistrarDelay            time.Duration
	ConjureDecoyRegistration            bool
	ConjureDecoyRegistrarDelay          time.Duration
	ConjureDecoyRegistrarWidth          int
	ConjureTransport                    string
	ConjureSTUNServerAddress            string
	ConjureDTLSEmptyInitialPacket       bool

	LivenessTestSeed *prng.Seed

	APIRequestPaddingSeed *prng.Seed

	HoldOffTunnelDuration time.Duration

	DialConnMetrics          common.MetricsSource       `json:"-"`
	DialConnNoticeMetrics    common.NoticeMetricsSource `json:"-"`
	ObfuscatedSSHConnMetrics common.MetricsSource       `json:"-"`

	DialDuration time.Duration `json:"-"`

	resolver          *resolver.Resolver `json:"-"`
	ResolveParameters *resolver.ResolveParameters

	HTTPTransformerParameters *transforms.HTTPTransformerParameters

	dialConfig *DialConfig `json:"-"`
	meekConfig *MeekConfig `json:"-"`
}

// MakeDialParameters creates a new DialParameters for the candidate server
// entry, including selecting a protocol and all the various protocol
// attributes. The input selectProtocol is used to comply with any active
// protocol selection constraints.
//
// When stored dial parameters are available and may be used,
// MakeDialParameters may replay previous dial parameters in an effort to
// leverage "known working" values instead of always chosing at random from a
// large space.
//
// MakeDialParameters will return nil/nil in cases where the candidate server
// entry should be skipped.
//
// To support replay, the caller must call DialParameters.Succeeded when a
// successful tunnel is established with the returned DialParameters; and must
// call DialParameters.Failed when a tunnel dial or activation fails, except
// when establishment is cancelled.
func MakeDialParameters(
	config *Config,
	upstreamProxyErrorCallback func(error),
	canReplay func(serverEntry *protocol.ServerEntry, replayProtocol string) bool,
	selectProtocol func(serverEntry *protocol.ServerEntry) (string, bool),
	serverEntry *protocol.ServerEntry,
	isTactics bool,
	candidateNumber int,
	establishedTunnelsCount int) (*DialParameters, error) {

	networkID := config.GetNetworkID()

	p := config.GetParameters().Get()

	ttl := p.Duration(parameters.ReplayDialParametersTTL)
	replayIgnoreChangedConfigState := p.Bool(parameters.ReplayIgnoreChangedConfigState)
	replayBPF := p.Bool(parameters.ReplayBPF)
	replaySSH := p.Bool(parameters.ReplaySSH)
	replayObfuscatorPadding := p.Bool(parameters.ReplayObfuscatorPadding)
	replayFragmentor := p.Bool(parameters.ReplayFragmentor)
	replayTLSProfile := p.Bool(parameters.ReplayTLSProfile)
	replayTLSFragmentClientHello := p.Bool(parameters.ReplayTLSFragmentClientHello)
	replayFronting := p.Bool(parameters.ReplayFronting)
	replayHostname := p.Bool(parameters.ReplayHostname)
	replayQUICVersion := p.Bool(parameters.ReplayQUICVersion)
	replayObfuscatedQUIC := p.Bool(parameters.ReplayObfuscatedQUIC)
	replayObfuscatedQUICNonceTransformer := p.Bool(parameters.ReplayObfuscatedQUICNonceTransformer)
	replayConjureRegistration := p.Bool(parameters.ReplayConjureRegistration)
	replayConjureTransport := p.Bool(parameters.ReplayConjureTransport)
	replayLivenessTest := p.Bool(parameters.ReplayLivenessTest)
	replayUserAgent := p.Bool(parameters.ReplayUserAgent)
	replayAPIRequestPadding := p.Bool(parameters.ReplayAPIRequestPadding)
	replayHoldOffTunnel := p.Bool(parameters.ReplayHoldOffTunnel)
	replayResolveParameters := p.Bool(parameters.ReplayResolveParameters)
	replayHTTPTransformerParameters := p.Bool(parameters.ReplayHTTPTransformerParameters)
	replayOSSHSeedTransformerParameters := p.Bool(parameters.ReplayOSSHSeedTransformerParameters)
	replayOSSHPrefix := p.Bool(parameters.ReplayOSSHPrefix)

	// Check for existing dial parameters for this server/network ID.

	dialParams, err := GetDialParameters(
		config, serverEntry.IpAddress, networkID)
	if err != nil {
		NoticeWarning("GetDialParameters failed: %s", err)
		dialParams = nil
		// Proceed, without existing dial parameters.
	}

	// Check if replay is permitted:
	// - TTL must be > 0 and existing dial parameters must not have expired
	//   as indicated by LastUsedTimestamp + TTL.
	// - Config/tactics/server entry values must be unchanged from when
	//   previous dial parameters were established.
	// - The protocol selection constraints must permit replay, as indicated
	//   by canReplay.
	// - Must not be using an obsolete TLS profile that is no longer supported.
	// - Must be using the latest Conjure API URL.
	//
	// When existing dial parameters don't meet these conditions, dialParams
	// is reset to nil and new dial parameters will be generated.

	var currentTimestamp time.Time
	var configStateHash []byte
	var serverEntryHash []byte

	// When TTL is 0, replay is disabled; the timestamp remains 0 and the
	// output DialParameters will not be stored by Success.

	if ttl > 0 {
		currentTimestamp = time.Now()
		configStateHash, serverEntryHash = getDialStateHashes(config, p, serverEntry)
	}

	if dialParams != nil &&
		(ttl <= 0 ||
			dialParams.LastUsedTimestamp.Before(currentTimestamp.Add(-ttl)) ||

			// Replay is disabled when the current config state hash -- config
			// dial parameters and the current tactics tag -- have changed
			// since the last dial. This prioritizes applying any potential
			// tactics change over redialing with parameters that may have
			// changed in tactics.
			//
			// Because of this, frequent tactics changes may degrade replay
			// effectiveness. When ReplayIgnoreChangedConfigState is set,
			// differences in the config state hash are ignored.
			//
			// Limitation: some code which previously assumed that replay
			// always implied unchanged tactics parameters may now use newer
			// tactics parameters in replay cases when
			// ReplayIgnoreChangedConfigState is set. One case is the call
			// below to fragmentor.NewUpstreamConfig, made when initializing
			// dialParams.dialConfig.
			(!replayIgnoreChangedConfigState && !bytes.Equal(dialParams.LastUsedConfigStateHash, configStateHash)) ||

			// Replay is disabled when the server entry has changed.
			!bytes.Equal(dialParams.LastUsedServerEntryHash, serverEntryHash) ||

			(dialParams.TLSProfile != "" &&
				!common.Contains(protocol.SupportedTLSProfiles, dialParams.TLSProfile)) ||
			(dialParams.QUICVersion != "" &&
				!common.Contains(protocol.SupportedQUICVersions, dialParams.QUICVersion)) ||

			// Legacy clients use ConjureAPIRegistrarURL with
			// gotapdance.tapdance.APIRegistrar and new clients use
			// ConjureAPIRegistrarBidirectionalURL with
			// gotapdance.tapdance.APIRegistrarBidirectional. Updated clients
			// may have replay dial parameters with the old
			// ConjureAPIRegistrarURL field, which is now ignored. In this
			// case, ConjureAPIRegistrarBidirectionalURL will be blank. Reset
			// this replay.
			(dialParams.ConjureAPIRegistration && dialParams.ConjureAPIRegistrarBidirectionalURL == "")) {

		// In these cases, existing dial parameters are expired or no longer
		// match the config state and so are cleared to avoid rechecking them.

		err = DeleteDialParameters(serverEntry.IpAddress, networkID)
		if err != nil {
			NoticeWarning("DeleteDialParameters failed: %s", err)
		}
		dialParams = nil
	}

	if dialParams != nil {
		if config.DisableReplay ||
			!canReplay(serverEntry, dialParams.TunnelProtocol) {

			// In these ephemeral cases, existing dial parameters may still be valid
			// and used in future establishment phases, and so are retained.

			dialParams = nil
		}
	}

	// IsExchanged:
	//
	// Dial parameters received via client-to-client exchange are partially
	// initialized. Only the exchange fields are retained, and all other dial
	// parameters fields must be initialized. This is not considered or logged as
	// a replay. The exchange case is identified by the IsExchanged flag.
	//
	// When previously stored, IsExchanged dial parameters will have set the same
	// timestamp and state hash used for regular dial parameters and the same
	// logic above should invalidate expired or invalid exchanged dial
	// parameters.
	//
	// Limitation: metrics will indicate when an exchanged server entry is used
	// (source "EXCHANGED") but will not indicate when exchanged dial parameters
	// are used vs. a redial after discarding dial parameters.

	isReplay := (dialParams != nil)
	isExchanged := isReplay && dialParams.IsExchanged

	if !isReplay {
		dialParams = &DialParameters{}
	}

	// Point to the current resolver to be used in dials.
	dialParams.resolver = config.GetResolver()
	if dialParams.resolver == nil {
		return nil, errors.TraceNew("missing resolver")
	}

	if isExchanged {
		// Set isReplay to false to cause all non-exchanged values to be
		// initialized; this also causes the exchange case to not log as replay.
		isReplay = false
	}

	// Set IsExchanged such that full dial parameters are stored and replayed
	// upon success.
	dialParams.IsExchanged = false

	dialParams.ServerEntry = serverEntry
	dialParams.NetworkID = networkID
	dialParams.IsReplay = isReplay
	dialParams.CandidateNumber = candidateNumber
	dialParams.EstablishedTunnelsCount = establishedTunnelsCount

	// Even when replaying, LastUsedTimestamp is updated to extend the TTL of
	// replayed dial parameters which will be updated in the datastore upon
	// success.

	dialParams.LastUsedTimestamp = currentTimestamp
	dialParams.LastUsedConfigStateHash = configStateHash
	dialParams.LastUsedServerEntryHash = serverEntryHash

	// Initialize dial parameters.
	//
	// When not replaying, all required parameters are initialized. When
	// replaying, existing parameters are retained, subject to the replay-X
	// tactics flags.

	// Select a network latency multiplier for this dial. This allows clients to
	// explore and discover timeout values appropriate for the current network.
	// The selection applies per tunnel, to avoid delaying all establishment
	// candidates due to excessive timeouts. The random selection is bounded by a
	// min/max set in tactics and an exponential distribution is used so as to
	// heavily favor values close to the min, which should be set to the
	// singleton NetworkLatencyMultiplier tactics value.
	//
	// For NetworkLatencyMultiplierLambda close to 2.0, values near min are
	// very approximately 10x more likely to be selected than values near
	// max, while for NetworkLatencyMultiplierLambda close to 0.1, the
	// distribution is close to uniform.
	//
	// Not all existing, persisted DialParameters will have a custom
	// NetworkLatencyMultiplier value. Its zero value will cause the singleton
	// NetworkLatencyMultiplier tactics value to be used instead, which is
	// consistent with the pre-custom multiplier behavior in the older client
	// version which persisted that DialParameters.

	networkLatencyMultiplierMin := p.Float(parameters.NetworkLatencyMultiplierMin)
	networkLatencyMultiplierMax := p.Float(parameters.NetworkLatencyMultiplierMax)

	if !isReplay ||
		// Was selected...
		(dialParams.NetworkLatencyMultiplier != 0.0 &&
			//  But is now outside tactics range...
			(dialParams.NetworkLatencyMultiplier < networkLatencyMultiplierMin ||
				dialParams.NetworkLatencyMultiplier > networkLatencyMultiplierMax)) {

		dialParams.NetworkLatencyMultiplier = prng.ExpFloat64Range(
			networkLatencyMultiplierMin,
			networkLatencyMultiplierMax,
			p.Float(parameters.NetworkLatencyMultiplierLambda))
	}

	// After this point, any tactics parameters that apply the network latency
	// multiplier will use this selected value.
	p = config.GetParameters().GetCustom(dialParams.NetworkLatencyMultiplier)

	if !isReplay && !isExchanged {

		// TODO: should there be a pre-check of selectProtocol before incurring
		// overhead of unmarshaling dial parameters? In may be that a server entry
		// is fully incapable of satisfying the current protocol selection
		// constraints.

		selectedProtocol, ok := selectProtocol(serverEntry)
		if !ok {
			return nil, nil
		}

		dialParams.TunnelProtocol = selectedProtocol
	}

	// Skip this candidate when the clients tactics restrict usage of the
	// fronting provider ID. See the corresponding server-side enforcement
	// comments in server.TacticsListener.accept.
	if protocol.TunnelProtocolUsesFrontedMeek(dialParams.TunnelProtocol) &&
		common.Contains(
			p.Strings(parameters.RestrictFrontingProviderIDs),
			dialParams.ServerEntry.FrontingProviderID) {
		if p.WeightedCoinFlip(
			parameters.RestrictFrontingProviderIDsClientProbability) {

			// When skipping, return nil/nil as no error should be logged.
			// NoticeSkipServerEntry emits each skip reason, regardless
			// of server entry, at most once per session.

			NoticeSkipServerEntry(
				"restricted fronting provider ID: %s",
				dialParams.ServerEntry.FrontingProviderID)

			return nil, nil
		}
	}

	if config.UseUpstreamProxy() {

		// When UpstreamProxy is configured, ServerEntry.GetSupportedProtocols, when
		// called via selectProtocol, will filter out protocols such that will not
		// select a protocol incompatible with UpstreamProxy. This additional check
		// will catch cases where selectProtocol does not apply this filter.
		if !protocol.TunnelProtocolSupportsUpstreamProxy(dialParams.TunnelProtocol) {

			NoticeSkipServerEntry(
				"protocol does not support upstream proxy: %s",
				dialParams.TunnelProtocol)

			return nil, nil
		}

		// Skip this candidate when the server entry is not to be used with an
		// upstream proxy. By not exposing servers from sources that are
		// relatively hard to enumerate, this mechanism mitigates the risk of
		// a malicious upstream proxy enumerating Psiphon servers. Populate
		// the allowed sources with fronted servers to provide greater
		// blocking resistence for clients using upstream proxy clients that
		// are subject to blocking.
		source := dialParams.ServerEntry.LocalSource
		if !protocol.AllowServerEntrySourceWithUpstreamProxy(source) &&
			!p.Bool(parameters.UpstreamProxyAllowAllServerEntrySources) {

			NoticeSkipServerEntry(
				"server entry source disallowed with upstream proxy: %s",
				source)

			return nil, nil
		}
	}

	if (!isReplay || !replayBPF) &&
		ClientBPFEnabled() &&
		protocol.TunnelProtocolUsesTCP(dialParams.TunnelProtocol) {

		if p.WeightedCoinFlip(parameters.BPFClientTCPProbability) {
			dialParams.BPFProgramName = ""
			dialParams.BPFProgramInstructions = nil
			ok, name, rawInstructions := p.BPFProgram(parameters.BPFClientTCPProgram)
			if ok {
				dialParams.BPFProgramName = name
				dialParams.BPFProgramInstructions = rawInstructions
			}
		}
	}

	if !isReplay || !replaySSH {
		dialParams.SelectedSSHClientVersion = true
		dialParams.SSHClientVersion = values.GetSSHClientVersion()
		dialParams.SSHKEXSeed, err = prng.NewSeed()
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	if !isReplay || !replayObfuscatorPadding {
		dialParams.ObfuscatorPaddingSeed, err = prng.NewSeed()
		if err != nil {
			return nil, errors.Trace(err)
		}

		if protocol.TunnelProtocolUsesMeek(dialParams.TunnelProtocol) {
			dialParams.MeekObfuscatorPaddingSeed, err = prng.NewSeed()
			if err != nil {
				return nil, errors.Trace(err)
			}
		} else if protocol.TunnelProtocolUsesTLSOSSH(dialParams.TunnelProtocol) {
			dialParams.TLSOSSHObfuscatorPaddingSeed, err = prng.NewSeed()
			if err != nil {
				return nil, errors.Trace(err)
			}
		}
	}

	if !isReplay || !replayFragmentor {
		dialParams.FragmentorSeed, err = prng.NewSeed()
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	if (!isReplay || !replayConjureRegistration) &&
		protocol.TunnelProtocolUsesConjure(dialParams.TunnelProtocol) {

		dialParams.ConjureCachedRegistrationTTL = p.Duration(parameters.ConjureCachedRegistrationTTL)

		apiURL := p.String(parameters.ConjureAPIRegistrarBidirectionalURL)
		decoyWidth := p.Int(parameters.ConjureDecoyRegistrarWidth)

		dialParams.ConjureAPIRegistration = apiURL != ""
		dialParams.ConjureDecoyRegistration = decoyWidth != 0

		// We select only one of API or decoy registration. When both are enabled,
		// ConjureDecoyRegistrarProbability determines the probability of using
		// decoy registration.
		//
		// In general, we disable retries in gotapdance and rely on Psiphon
		// establishment to try/retry different registration schemes. This allows us
		// to control the proportion of registration types attempted. And, in good
		// network conditions, individual candidates are most likely to be cancelled
		// before they exhaust their retry options.

		if dialParams.ConjureAPIRegistration && dialParams.ConjureDecoyRegistration {
			if p.WeightedCoinFlip(parameters.ConjureDecoyRegistrarProbability) {
				dialParams.ConjureAPIRegistration = false
			}
		}

		if dialParams.ConjureAPIRegistration {

			// While Conjure API registration uses MeekConn and specifies common meek
			// parameters, the meek address and SNI configuration is implemented in this
			// code block and not in common code blocks below. The exception is TLS
			// configuration.
			//
			// Accordingly, replayFronting/replayHostname have no effect on Conjure API
			// registration replay.

			dialParams.ConjureAPIRegistrarBidirectionalURL = apiURL

			frontingSpecs := p.FrontingSpecs(parameters.ConjureAPIRegistrarFrontingSpecs)
			dialParams.FrontingProviderID,
				dialParams.MeekFrontingDialAddress,
				dialParams.MeekSNIServerName,
				dialParams.MeekVerifyServerName,
				dialParams.MeekVerifyPins,
				dialParams.MeekFrontingHost,
				err = frontingSpecs.SelectParameters()
			if err != nil {
				return nil, errors.Trace(err)
			}

			if config.DisableSystemRootCAs {
				return nil, errors.TraceNew("TLS certificates must be verified in Conjure API registration")
			}

			dialParams.MeekDialAddress = net.JoinHostPort(dialParams.MeekFrontingDialAddress, "443")
			dialParams.MeekHostHeader = dialParams.MeekFrontingHost

			// For a FrontingSpec, an SNI value of "" indicates to disable/omit SNI, so
			// never transform in that case.
			if dialParams.MeekSNIServerName != "" {
				if p.WeightedCoinFlip(parameters.TransformHostNameProbability) {
					dialParams.MeekSNIServerName = selectHostName(dialParams.TunnelProtocol, p)
					dialParams.MeekTransformedHostName = true
				}
			}

			// The minimum delay value is determined by the Conjure station, which
			// performs an asynchronous "liveness test" against the selected phantom
			// IPs. The min/max range allows us to introduce some jitter so that we
			// don't present a trivial inter-flow fingerprint: CDN connection, fixed
			// delay, phantom dial.

			minDelay := p.Duration(parameters.ConjureAPIRegistrarMinDelay)
			maxDelay := p.Duration(parameters.ConjureAPIRegistrarMaxDelay)
			dialParams.ConjureAPIRegistrarDelay = prng.Period(minDelay, maxDelay)

		} else if dialParams.ConjureDecoyRegistration {

			dialParams.ConjureDecoyRegistrarWidth = decoyWidth
			minDelay := p.Duration(parameters.ConjureDecoyRegistrarMinDelay)
			maxDelay := p.Duration(parameters.ConjureDecoyRegistrarMaxDelay)
			dialParams.ConjureAPIRegistrarDelay = prng.Period(minDelay, maxDelay)

		} else {

			return nil, errors.TraceNew("no Conjure registrar configured")
		}
	}

	if (!isReplay || !replayConjureTransport) &&
		protocol.TunnelProtocolUsesConjure(dialParams.TunnelProtocol) {

		// None of ConjureEnableIPv6Dials, ConjureEnablePortRandomization, or
		// ConjureEnableRegistrationOverrides are set here for replay. The
		// current value of these flag parameters is always applied.

		dialParams.ConjureTransport = selectConjureTransport(p)
		if protocol.ConjureTransportUsesSTUN(dialParams.ConjureTransport) {
			stunServerAddresses := p.Strings(parameters.ConjureSTUNServerAddresses)
			if len(stunServerAddresses) == 0 {
				return nil, errors.Tracef(
					"no Conjure STUN servers addresses configured for transport %s", dialParams.ConjureTransport)
			}
			dialParams.ConjureSTUNServerAddress = stunServerAddresses[prng.Intn(len(stunServerAddresses))]
			dialParams.ConjureDTLSEmptyInitialPacket = p.WeightedCoinFlip(
				parameters.ConjureDTLSEmptyInitialPacketProbability)
		}
	}

	usingTLS := protocol.TunnelProtocolUsesMeekHTTPS(dialParams.TunnelProtocol) ||
		protocol.TunnelProtocolUsesTLSOSSH(dialParams.TunnelProtocol) ||
		dialParams.ConjureAPIRegistration

	if (!isReplay || !replayTLSProfile) && usingTLS {

		dialParams.SelectedTLSProfile = true

		requireTLS12SessionTickets := protocol.TunnelProtocolRequiresTLS12SessionTickets(
			dialParams.TunnelProtocol)

		requireTLS13Support := protocol.TunnelProtocolRequiresTLS13Support(dialParams.TunnelProtocol)

		isFronted := protocol.TunnelProtocolUsesFrontedMeek(dialParams.TunnelProtocol) ||
			dialParams.ConjureAPIRegistration

		dialParams.TLSProfile, dialParams.TLSVersion, dialParams.RandomizedTLSProfileSeed, err = SelectTLSProfile(
			requireTLS12SessionTickets, requireTLS13Support, isFronted, serverEntry.FrontingProviderID, p)
		if err != nil {
			return nil, errors.Trace(err)
		}

		if dialParams.TLSProfile == "" && (requireTLS12SessionTickets || requireTLS13Support) {
			return nil, errors.TraceNew("required TLS profile not found")
		}

		dialParams.NoDefaultTLSSessionID = p.WeightedCoinFlip(
			parameters.NoDefaultTLSSessionIDProbability)
	}

	if (!isReplay || !replayFronting) &&
		protocol.TunnelProtocolUsesFrontedMeek(dialParams.TunnelProtocol) {

		dialParams.FrontingProviderID = serverEntry.FrontingProviderID

		dialParams.MeekFrontingDialAddress, dialParams.MeekFrontingHost, err =
			selectFrontingParameters(serverEntry)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	if !isReplay || !replayHostname {

		// Any MeekHostHeader selections made here will be overridden below,
		// as required, for fronting cases.

		if protocol.TunnelProtocolUsesMeekHTTPS(dialParams.TunnelProtocol) ||
			protocol.TunnelProtocolUsesFrontedMeekQUIC(dialParams.TunnelProtocol) {

			dialParams.MeekSNIServerName = ""
			hostname := ""
			if p.WeightedCoinFlip(parameters.TransformHostNameProbability) {
				dialParams.MeekSNIServerName = selectHostName(dialParams.TunnelProtocol, p)
				hostname = dialParams.MeekSNIServerName
				dialParams.MeekTransformedHostName = true
			} else {

				// Always select a hostname for the Host header in this case.
				// Unlike HTTP, the Host header isn't plaintext on the wire,
				// and so there's no anti-fingerprint benefit from presenting
				// the server IP address in the Host header. Omitting the
				// server IP here can prevent exposing it in certain
				// scenarios where the traffic is rerouted and arrives at a
				// different HTTPS server.

				hostname = selectHostName(dialParams.TunnelProtocol, p)
			}
			if serverEntry.MeekServerPort == 443 {
				dialParams.MeekHostHeader = hostname
			} else {
				dialParams.MeekHostHeader = net.JoinHostPort(
					hostname, strconv.Itoa(serverEntry.MeekServerPort))
			}

		} else if protocol.TunnelProtocolUsesTLSOSSH(dialParams.TunnelProtocol) {

			dialParams.TLSOSSHSNIServerName = ""
			if p.WeightedCoinFlip(parameters.TransformHostNameProbability) {
				dialParams.TLSOSSHSNIServerName = selectHostName(dialParams.TunnelProtocol, p)
				dialParams.TLSOSSHTransformedSNIServerName = true
			}

		} else if protocol.TunnelProtocolUsesMeekHTTP(dialParams.TunnelProtocol) {

			dialParams.MeekHostHeader = ""
			hostname := serverEntry.IpAddress
			if p.WeightedCoinFlip(parameters.TransformHostNameProbability) {
				hostname = selectHostName(dialParams.TunnelProtocol, p)
				dialParams.MeekTransformedHostName = true
			}
			if serverEntry.MeekServerPort == 80 {
				dialParams.MeekHostHeader = hostname
			} else {
				dialParams.MeekHostHeader = net.JoinHostPort(
					hostname, strconv.Itoa(serverEntry.MeekServerPort))
			}
		} else if protocol.TunnelProtocolUsesQUIC(dialParams.TunnelProtocol) {
			dialParams.QUICDialSNIAddress = selectHostName(dialParams.TunnelProtocol, p)
		}
	}

	if (!isReplay || !replayQUICVersion) &&
		protocol.TunnelProtocolUsesQUIC(dialParams.TunnelProtocol) {

		isFronted := protocol.TunnelProtocolUsesFrontedMeekQUIC(dialParams.TunnelProtocol)
		dialParams.QUICVersion = selectQUICVersion(isFronted, serverEntry, p)

		// Due to potential tactics configurations, it may be that no QUIC
		// version is selected. Abort immediately, with no error, as in the
		// selectProtocol case. quic.Dial and quic.NewQUICTransporter will
		// check for a missing QUIC version, but at that later stage an
		// unnecessary failed_tunnel can be logged in this scenario.
		if dialParams.QUICVersion == "" {
			return nil, nil
		}

		if protocol.QUICVersionHasRandomizedClientHello(dialParams.QUICVersion) {
			dialParams.QUICClientHelloSeed, err = prng.NewSeed()
			if err != nil {
				return nil, errors.Trace(err)
			}
		}

		dialParams.QUICDisablePathMTUDiscovery =
			protocol.QUICVersionUsesPathMTUDiscovery(dialParams.QUICVersion) &&
				p.WeightedCoinFlip(parameters.QUICDisableClientPathMTUDiscoveryProbability)
	}

	if (!isReplay || !replayObfuscatedQUIC) &&
		protocol.QUICVersionIsObfuscated(dialParams.QUICVersion) {

		dialParams.ObfuscatedQUICPaddingSeed, err = prng.NewSeed()
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	if protocol.QUICVersionIsObfuscated(dialParams.QUICVersion) {

		if serverEntry.DisableObfuscatedQUICTransforms {

			dialParams.ObfuscatedQUICNonceTransformerParameters = nil

		} else if !isReplay || !replayObfuscatedQUICNonceTransformer {

			params, err := makeSeedTransformerParameters(
				p,
				parameters.ObfuscatedQUICNonceTransformProbability,
				parameters.ObfuscatedQUICNonceTransformSpecs,
				parameters.ObfuscatedQUICNonceTransformScopedSpecNames)
			if err != nil {
				return nil, errors.Trace(err)
			}

			if params.TransformSpec != nil {
				dialParams.ObfuscatedQUICNonceTransformerParameters = params
			} else {
				dialParams.ObfuscatedQUICNonceTransformerParameters = nil
			}
		}
	}

	if !isReplay || !replayLivenessTest {

		// TODO: initialize only when LivenessTestMaxUp/DownstreamBytes > 0?
		dialParams.LivenessTestSeed, err = prng.NewSeed()
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	if !isReplay || !replayAPIRequestPadding {
		dialParams.APIRequestPaddingSeed, err = prng.NewSeed()
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	// Initialize dialParams.ResolveParameters for dials that will resolve
	// domain names, which currently includes fronted meek and Conjure API
	// registration, where the dial address is not an IP address.
	//
	// dialParams.ResolveParameters must be nil when the dial address is an IP
	// address to ensure that no DNS dial parameters are reported in metrics
	// or diagnostics when when no domain is resolved.

	useResolver := (protocol.TunnelProtocolUsesFrontedMeek(dialParams.TunnelProtocol) ||
		dialParams.ConjureAPIRegistration) &&
		net.ParseIP(dialParams.MeekFrontingDialAddress) == nil

	if (!isReplay || !replayResolveParameters) && useResolver {

		dialParams.ResolveParameters, err = dialParams.resolver.MakeResolveParameters(
			p, dialParams.FrontingProviderID, dialParams.MeekFrontingDialAddress)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	if !isReplay || !replayHoldOffTunnel {

		if common.Contains(
			p.TunnelProtocols(parameters.HoldOffTunnelProtocols), dialParams.TunnelProtocol) ||

			(protocol.TunnelProtocolUsesFrontedMeek(dialParams.TunnelProtocol) &&
				common.Contains(
					p.Strings(parameters.HoldOffTunnelFrontingProviderIDs),
					dialParams.FrontingProviderID)) {

			if p.WeightedCoinFlip(parameters.HoldOffTunnelProbability) {

				dialParams.HoldOffTunnelDuration = prng.Period(
					p.Duration(parameters.HoldOffTunnelMinDuration),
					p.Duration(parameters.HoldOffTunnelMaxDuration))
			}
		}

	}

	// OSSH prefix and seed transform are applied only to the OSSH tunnel protocol,
	// and not to any other protocol layered over OSSH.
	if dialParams.TunnelProtocol == protocol.TUNNEL_PROTOCOL_OBFUSCATED_SSH {

		if serverEntry.DisableOSSHTransforms {

			dialParams.OSSHObfuscatorSeedTransformerParameters = nil

		} else if !isReplay || !replayOSSHSeedTransformerParameters {

			params, err := makeSeedTransformerParameters(
				p,
				parameters.OSSHObfuscatorSeedTransformProbability,
				parameters.OSSHObfuscatorSeedTransformSpecs,
				parameters.OSSHObfuscatorSeedTransformScopedSpecNames)
			if err != nil {
				return nil, errors.Trace(err)
			}

			if params.TransformSpec != nil {
				dialParams.OSSHObfuscatorSeedTransformerParameters = params
			} else {
				dialParams.OSSHObfuscatorSeedTransformerParameters = nil
			}
		}

		if serverEntry.DisableOSSHPrefix {
			dialParams.OSSHPrefixSpec = nil
			dialParams.OSSHPrefixSplitConfig = nil

		} else if !isReplay || !replayOSSHPrefix {

			dialPortNumber, err := serverEntry.GetDialPortNumber(dialParams.TunnelProtocol)
			if err != nil {
				return nil, errors.Trace(err)
			}
			prefixSpec, err := makeOSSHPrefixSpecParameters(p, strconv.Itoa(dialPortNumber))
			if err != nil {
				return nil, errors.Trace(err)
			}

			splitConfig, err := makeOSSHPrefixSplitConfig(p)
			if err != nil {
				return nil, errors.Trace(err)
			}

			if prefixSpec.Spec != nil {
				dialParams.OSSHPrefixSpec = prefixSpec
				dialParams.OSSHPrefixSplitConfig = splitConfig
			} else {
				dialParams.OSSHPrefixSpec = nil
				dialParams.OSSHPrefixSplitConfig = nil
			}
		}

		// OSSHPrefix supersedes OSSHObfuscatorSeedTransform.
		// This ensures both tactics are not used simultaneously,
		// until OSSHObfuscatorSeedTransform is removed.
		if dialParams.OSSHPrefixSpec != nil {
			dialParams.OSSHObfuscatorSeedTransformerParameters = nil
		}

	}

	if protocol.TunnelProtocolUsesMeekHTTP(dialParams.TunnelProtocol) {

		if serverEntry.DisableHTTPTransforms {

			dialParams.HTTPTransformerParameters = nil

		} else if !isReplay || !replayHTTPTransformerParameters {

			isFronted := protocol.TunnelProtocolUsesFrontedMeek(dialParams.TunnelProtocol)

			params, err := makeHTTPTransformerParameters(config.GetParameters().Get(), serverEntry.FrontingProviderID, isFronted)
			if err != nil {
				return nil, errors.Trace(err)
			}

			if params.ProtocolTransformSpec != nil {
				dialParams.HTTPTransformerParameters = params
			} else {
				dialParams.HTTPTransformerParameters = nil
			}
		}
	}

	// Set dial address fields. This portion of configuration is
	// deterministic, given the parameters established or replayed so far.

	dialPortNumber, err := serverEntry.GetDialPortNumber(dialParams.TunnelProtocol)
	if err != nil {
		return nil, errors.Trace(err)
	}

	dialParams.DialPortNumber = strconv.Itoa(dialPortNumber)

	switch dialParams.TunnelProtocol {

	case protocol.TUNNEL_PROTOCOL_SSH,
		protocol.TUNNEL_PROTOCOL_OBFUSCATED_SSH,
		protocol.TUNNEL_PROTOCOL_TAPDANCE_OBFUSCATED_SSH,
		protocol.TUNNEL_PROTOCOL_CONJURE_OBFUSCATED_SSH,
		protocol.TUNNEL_PROTOCOL_QUIC_OBFUSCATED_SSH,
		protocol.TUNNEL_PROTOCOL_TLS_OBFUSCATED_SSH:

		dialParams.DirectDialAddress = net.JoinHostPort(serverEntry.IpAddress, dialParams.DialPortNumber)

	case protocol.TUNNEL_PROTOCOL_FRONTED_MEEK,
		protocol.TUNNEL_PROTOCOL_FRONTED_MEEK_QUIC_OBFUSCATED_SSH:

		dialParams.MeekDialAddress = net.JoinHostPort(dialParams.MeekFrontingDialAddress, dialParams.DialPortNumber)
		dialParams.MeekHostHeader = dialParams.MeekFrontingHost
		if serverEntry.MeekFrontingDisableSNI {
			dialParams.MeekSNIServerName = ""
			// When SNI is omitted, the transformed host name is not used.
			dialParams.MeekTransformedHostName = false
		} else if !dialParams.MeekTransformedHostName {
			dialParams.MeekSNIServerName = dialParams.MeekFrontingDialAddress
		}

	case protocol.TUNNEL_PROTOCOL_FRONTED_MEEK_HTTP:

		dialParams.MeekDialAddress = net.JoinHostPort(dialParams.MeekFrontingDialAddress, dialParams.DialPortNumber)
		dialParams.MeekHostHeader = dialParams.MeekFrontingHost
		// For FRONTED HTTP, the Host header cannot be transformed.
		dialParams.MeekTransformedHostName = false

	case protocol.TUNNEL_PROTOCOL_UNFRONTED_MEEK:

		dialParams.MeekDialAddress = net.JoinHostPort(serverEntry.IpAddress, dialParams.DialPortNumber)

	case protocol.TUNNEL_PROTOCOL_UNFRONTED_MEEK_HTTPS,
		protocol.TUNNEL_PROTOCOL_UNFRONTED_MEEK_SESSION_TICKET:

		dialParams.MeekDialAddress = net.JoinHostPort(serverEntry.IpAddress, dialParams.DialPortNumber)
		if !dialParams.MeekTransformedHostName {
			// Note: IP address in SNI field will be omitted.
			dialParams.MeekSNIServerName = serverEntry.IpAddress
		}

	default:
		return nil, errors.Tracef(
			"unknown tunnel protocol: %s", dialParams.TunnelProtocol)
	}

	if protocol.TunnelProtocolUsesMeek(dialParams.TunnelProtocol) {

		host, _, _ := net.SplitHostPort(dialParams.MeekDialAddress)

		if p.Bool(parameters.MeekDialDomainsOnly) {
			if net.ParseIP(host) != nil {
				// No error, as this is a "not supported" case.
				return nil, nil
			}
		}

		// The underlying TLS implementation will automatically omit SNI for
		// IP address server name values; we have this explicit check here so
		// we record the correct value for stats.
		if net.ParseIP(dialParams.MeekSNIServerName) != nil {
			dialParams.MeekSNIServerName = ""
		}
	}

	// TLS ClientHello fragmentation is applied only after the state
	// of SNI is determined above.
	if (!isReplay || !replayTLSFragmentClientHello) && usingTLS {

		limitProtocols := p.TunnelProtocols(parameters.TLSFragmentClientHelloLimitProtocols)
		if len(limitProtocols) == 0 || common.Contains(limitProtocols, dialParams.TunnelProtocol) {

			// Note: The TLS stack automatically drops the SNI extension when
			// the host is an IP address.

			usingSNI := false
			if dialParams.TLSOSSHSNIServerName != "" {
				usingSNI = net.ParseIP(dialParams.TLSOSSHSNIServerName) == nil

			} else if dialParams.MeekSNIServerName != "" {
				usingSNI = net.ParseIP(dialParams.MeekSNIServerName) == nil
			}

			// TLS ClientHello fragmentor expects SNI to be present.
			if usingSNI {
				dialParams.TLSFragmentClientHello = p.WeightedCoinFlip(
					parameters.TLSFragmentClientHelloProbability)
			}
		}
	}

	// Initialize/replay User-Agent header for HTTP upstream proxy and meek protocols.

	if config.UseUpstreamProxy() {
		// Note: UpstreamProxyURL will be validated in the dial
		proxyURL, err := common.SafeParseURL(config.UpstreamProxyURL)
		if err == nil {
			dialParams.UpstreamProxyType = proxyURL.Scheme
		}
	}

	dialCustomHeaders := makeDialCustomHeaders(config, p)

	if protocol.TunnelProtocolUsesMeek(dialParams.TunnelProtocol) ||
		dialParams.UpstreamProxyType == "http" ||
		dialParams.ConjureAPIRegistration {

		if !isReplay || !replayUserAgent {
			dialParams.SelectedUserAgent, dialParams.UserAgent = selectUserAgentIfUnset(p, dialCustomHeaders)
		}

		if dialParams.SelectedUserAgent {
			dialCustomHeaders.Set("User-Agent", dialParams.UserAgent)
		}

	}

	// UpstreamProxyCustomHeaderNames is a reported metric. Just the names and
	// not the values are reported, in case the values are identifying.

	if len(config.CustomHeaders) > 0 {
		dialParams.UpstreamProxyCustomHeaderNames = make([]string, 0)
		for name := range dialCustomHeaders {
			if name == "User-Agent" && dialParams.SelectedUserAgent {
				continue
			}
			dialParams.UpstreamProxyCustomHeaderNames = append(dialParams.UpstreamProxyCustomHeaderNames, name)
		}
	}

	// Initialize Dial/MeekConfigs to be passed to the corresponding dialers.

	// Custom ResolveParameters are set only when useResolver is true, but
	// DialConfig.ResolveIP is required and wired up unconditionally. Any
	// misconfigured or miscoded domain dial cases will use default
	// ResolveParameters.
	//
	// ResolveIP will use the networkID obtained above, as it will be used
	// almost immediately, instead of incurring the overhead of calling
	// GetNetworkID again.
	resolveIP := func(ctx context.Context, hostname string) ([]net.IP, error) {
		IPs, err := dialParams.resolver.ResolveIP(
			ctx,
			networkID,
			dialParams.ResolveParameters,
			hostname)
		if err != nil {
			return nil, errors.Trace(err)
		}
		return IPs, nil
	}

	// Fragmentor configuration.
	// Note: fragmentorConfig is nil if fragmentor is disabled for prefixed OSSH.
	//
	// Limitation: when replaying and with ReplayIgnoreChangedConfigState set,
	// fragmentor.NewUpstreamConfig may select a config using newer tactics
	// parameters.
	fragmentorConfig := fragmentor.NewUpstreamConfig(p, dialParams.TunnelProtocol, dialParams.FragmentorSeed)
	if !p.Bool(parameters.OSSHPrefixEnableFragmentor) && dialParams.OSSHPrefixSpec != nil {
		fragmentorConfig = nil
	}

	dialParams.dialConfig = &DialConfig{
		DiagnosticID:                  serverEntry.GetDiagnosticID(),
		UpstreamProxyURL:              config.UpstreamProxyURL,
		CustomHeaders:                 dialCustomHeaders,
		BPFProgramInstructions:        dialParams.BPFProgramInstructions,
		DeviceBinder:                  config.deviceBinder,
		IPv6Synthesizer:               config.IPv6Synthesizer,
		ResolveIP:                     resolveIP,
		TrustedCACertificatesFilename: config.TrustedCACertificatesFilename,
		FragmentorConfig:              fragmentorConfig,
		UpstreamProxyErrorCallback:    upstreamProxyErrorCallback,
	}

	// Unconditionally initialize MeekResolvedIPAddress, so a valid string can
	// always be read.
	dialParams.MeekResolvedIPAddress.Store("")

	if protocol.TunnelProtocolUsesMeek(dialParams.TunnelProtocol) ||
		dialParams.ConjureAPIRegistration {

		// For tactics requests, AddPsiphonFrontingHeader is set when set for
		// the related tunnel protocol. E.g., FRONTED-OSSH-MEEK for
		// FRONTED-MEEK-TACTICS. AddPsiphonFrontingHeader is not replayed.
		addPsiphonFrontingHeader := false
		if dialParams.FrontingProviderID != "" {
			addPsiphonFrontingHeader = common.Contains(
				p.LabeledTunnelProtocols(
					parameters.AddFrontingProviderPsiphonFrontingHeader, dialParams.FrontingProviderID),
				dialParams.TunnelProtocol)
		}

		dialParams.meekConfig = &MeekConfig{
			DiagnosticID:                  serverEntry.GetDiagnosticID(),
			Parameters:                    config.GetParameters(),
			DialAddress:                   dialParams.MeekDialAddress,
			UseQUIC:                       protocol.TunnelProtocolUsesFrontedMeekQUIC(dialParams.TunnelProtocol),
			QUICVersion:                   dialParams.QUICVersion,
			QUICClientHelloSeed:           dialParams.QUICClientHelloSeed,
			QUICDisablePathMTUDiscovery:   dialParams.QUICDisablePathMTUDiscovery,
			UseHTTPS:                      usingTLS,
			TLSProfile:                    dialParams.TLSProfile,
			TLSFragmentClientHello:        dialParams.TLSFragmentClientHello,
			LegacyPassthrough:             serverEntry.ProtocolUsesLegacyPassthrough(dialParams.TunnelProtocol),
			NoDefaultTLSSessionID:         dialParams.NoDefaultTLSSessionID,
			RandomizedTLSProfileSeed:      dialParams.RandomizedTLSProfileSeed,
			UseObfuscatedSessionTickets:   dialParams.TunnelProtocol == protocol.TUNNEL_PROTOCOL_UNFRONTED_MEEK_SESSION_TICKET,
			SNIServerName:                 dialParams.MeekSNIServerName,
			AddPsiphonFrontingHeader:      addPsiphonFrontingHeader,
			VerifyServerName:              dialParams.MeekVerifyServerName,
			VerifyPins:                    dialParams.MeekVerifyPins,
			DisableSystemRootCAs:          config.DisableSystemRootCAs,
			HostHeader:                    dialParams.MeekHostHeader,
			TransformedHostName:           dialParams.MeekTransformedHostName,
			ClientTunnelProtocol:          dialParams.TunnelProtocol,
			MeekCookieEncryptionPublicKey: serverEntry.MeekCookieEncryptionPublicKey,
			MeekObfuscatedKey:             serverEntry.MeekObfuscatedKey,
			MeekObfuscatorPaddingSeed:     dialParams.MeekObfuscatorPaddingSeed,
			NetworkLatencyMultiplier:      dialParams.NetworkLatencyMultiplier,
			HTTPTransformerParameters:     dialParams.HTTPTransformerParameters,
		}

		// Use an asynchronous callback to record the resolved IP address when
		// dialing a domain name. Note that DialMeek doesn't immediately
		// establish any HTTP connections, so the resolved IP address won't be
		// reported in all cases until after SSH traffic is relayed or a
		// endpoint request is made over the meek connection.
		dialParams.dialConfig.ResolvedIPCallback = func(IPAddress string) {
			dialParams.MeekResolvedIPAddress.Store(IPAddress)
		}

		if isTactics {
			dialParams.meekConfig.Mode = MeekModeObfuscatedRoundTrip
		} else if dialParams.ConjureAPIRegistration {
			dialParams.meekConfig.Mode = MeekModePlaintextRoundTrip
		} else {
			dialParams.meekConfig.Mode = MeekModeRelay
		}
	}

	return dialParams, nil
}

func (dialParams *DialParameters) GetDialConfig() *DialConfig {
	return dialParams.dialConfig
}

func (dialParams *DialParameters) GetTLSOSSHConfig(config *Config) *TLSTunnelConfig {

	return &TLSTunnelConfig{
		CustomTLSConfig: &CustomTLSConfig{
			Parameters:               config.GetParameters(),
			DialAddr:                 dialParams.DirectDialAddress,
			SNIServerName:            dialParams.TLSOSSHSNIServerName,
			SkipVerify:               true,
			VerifyServerName:         "",
			VerifyPins:               nil,
			TLSProfile:               dialParams.TLSProfile,
			NoDefaultTLSSessionID:    &dialParams.NoDefaultTLSSessionID,
			RandomizedTLSProfileSeed: dialParams.RandomizedTLSProfileSeed,
			FragmentClientHello:      dialParams.TLSFragmentClientHello,
		},
		// Obfuscated session tickets are not used because TLS-OSSH uses TLS 1.3.
		UseObfuscatedSessionTickets: false,
		// Meek obfuscated key used to allow clients with legacy unfronted
		// meek-https server entries, that have the passthrough capability, to
		// connect with TLS-OSSH to the servers corresponding to those server
		// entries, which now support TLS-OSSH by demultiplexing meek-https and
		// TLS-OSSH over the meek-https port.
		ObfuscatedKey:         dialParams.ServerEntry.MeekObfuscatedKey,
		ObfuscatorPaddingSeed: dialParams.TLSOSSHObfuscatorPaddingSeed,
	}
}

func (dialParams *DialParameters) GetMeekConfig() *MeekConfig {
	return dialParams.meekConfig
}

// GetNetworkType returns a network type name, suitable for metrics, which is
// derived from the network ID.
func (dialParams *DialParameters) GetNetworkType() string {

	// Unlike the logic in loggingNetworkIDGetter.GetNetworkID, we don't take the
	// arbitrary text before the first "-" since some platforms without network
	// detection support stub in random values to enable tactics. Instead we
	// check for and use the common network type prefixes currently used in
	// NetworkIDGetter implementations.

	if strings.HasPrefix(dialParams.NetworkID, "VPN") {
		return "VPN"
	}
	if strings.HasPrefix(dialParams.NetworkID, "WIFI") {
		return "WIFI"
	}
	if strings.HasPrefix(dialParams.NetworkID, "MOBILE") {
		return "MOBILE"
	}
	return "UNKNOWN"
}

func (dialParams *DialParameters) Succeeded() {

	// When TTL is 0, don't store dial parameters.
	if dialParams.LastUsedTimestamp.IsZero() {
		return
	}

	NoticeInfo("Set dial parameters for %s", dialParams.ServerEntry.GetDiagnosticID())
	err := SetDialParameters(dialParams.ServerEntry.IpAddress, dialParams.NetworkID, dialParams)
	if err != nil {
		NoticeWarning("SetDialParameters failed: %s", err)
	}
}

func (dialParams *DialParameters) Failed(config *Config) {

	// When a tunnel fails, and the dial is a replay, clear the stored dial
	// parameters which are now presumed to be blocked, impaired or otherwise
	// no longer effective.
	//
	// It may be the case that a dial is not using stored dial parameters
	// (!IsReplay), and in this case we retain those dial parameters since they
	// were not exercised and may still be effective.
	//
	// Failed tunnel dial parameters may be retained with a configurable
	// probability; this is intended to help mitigate false positive failures due
	// to, e.g., temporary network disruptions or server load limiting.

	if dialParams.IsReplay &&
		!config.GetParameters().Get().WeightedCoinFlip(
			parameters.ReplayRetainFailedProbability) {

		NoticeInfo("Delete dial parameters for %s", dialParams.ServerEntry.GetDiagnosticID())
		err := DeleteDialParameters(dialParams.ServerEntry.IpAddress, dialParams.NetworkID)
		if err != nil {
			NoticeWarning("DeleteDialParameters failed: %s", err)
		}
	}
}

func (dialParams *DialParameters) GetTLSVersionForMetrics() string {
	return getTLSVersionForMetrics(dialParams.TLSVersion, dialParams.NoDefaultTLSSessionID)
}

func getTLSVersionForMetrics(tlsVersion string, noDefaultTLSSessionID bool) string {
	version := tlsVersion
	if noDefaultTLSSessionID {
		version += "-no_def_id"
	}
	return version
}

// ExchangedDialParameters represents the subset of DialParameters that is
// shared in a client-to-client exchange of server connection info.
//
// The purpose of client-to-client exchange if for one user that can connect
// to help another user that cannot connect by sharing their connected
// configuration, including the server entry and dial parameters.
//
// There are two concerns regarding which dial parameter fields are safe to
// exchange:
//
//   - Unlike signed server entries, there's no independent trust anchor
//     that can certify that the exchange data is valid.
//
//   - While users should only perform the exchange with trusted peers,
//     the user's trust in their peer may be misplaced.
//
// This presents the possibility of attack such as the peer sending dial
// parameters that could be used to trace/monitor/flag the importer; or
// sending dial parameters, including dial address and SNI, to cause the peer
// to appear to connect to a banned service.
//
// To mitigate these risks, only a subset of dial parameters are exchanged.
// When exchanged dial parameters and imported and used, all unexchanged
// parameters are generated locally. At this time, only the tunnel protocol is
// exchanged. We consider tunnel protocol selection one of the key connection
// success factors.
//
// In addition, the exchange peers may not be on the same network with the
// same blocking and circumvention characteristics, which is another reason
// to limit exchanged dial parameter values to broadly applicable fields.
//
// Unlike the exchanged (and otherwise acquired) server entry,
// ExchangedDialParameters does not use the ServerEntry_Fields_ representation
// which allows older clients to receive and store new, unknown fields. Such a
// facility is less useful in this case, since exchanged dial parameters and
// used immediately and have a short lifespan.
//
// TODO: exchange more dial parameters, such as TLS profile, QUIC version, etc.
type ExchangedDialParameters struct {
	TunnelProtocol string
}

// NewExchangedDialParameters creates a new ExchangedDialParameters from a
// DialParameters, including only the exchanged values.
// NewExchangedDialParameters assumes the input DialParameters has been
// initialized and populated by MakeDialParameters.
func NewExchangedDialParameters(dialParams *DialParameters) *ExchangedDialParameters {
	return &ExchangedDialParameters{
		TunnelProtocol: dialParams.TunnelProtocol,
	}
}

// Validate checks that the ExchangedDialParameters contains only valid values
// and is compatible with the specified server entry.
func (dialParams *ExchangedDialParameters) Validate(serverEntry *protocol.ServerEntry) error {
	if !common.Contains(protocol.SupportedTunnelProtocols, dialParams.TunnelProtocol) {
		return errors.Tracef("unknown tunnel protocol: %s", dialParams.TunnelProtocol)
	}
	if !serverEntry.SupportsProtocol(dialParams.TunnelProtocol) {
		return errors.Tracef("unsupported tunnel protocol: %s", dialParams.TunnelProtocol)
	}
	return nil
}

// MakeDialParameters creates a new, partially intitialized DialParameters
// from the values in ExchangedDialParameters. The returned DialParameters
// must not be used directly for dialing. It is intended to be stored, and
// then later fully initialized by MakeDialParameters.
func (dialParams *ExchangedDialParameters) MakeDialParameters(
	config *Config,
	p parameters.ParametersAccessor,
	serverEntry *protocol.ServerEntry) *DialParameters {

	configStateHash, serverEntryHash := getDialStateHashes(config, p, serverEntry)

	return &DialParameters{
		IsExchanged:             true,
		LastUsedTimestamp:       time.Now(),
		LastUsedConfigStateHash: configStateHash,
		LastUsedServerEntryHash: serverEntryHash,
		TunnelProtocol:          dialParams.TunnelProtocol,
	}
}

// getDialStateHashes returns two hashes: the config state hash reflects the
// config dial parameters and tactics tag used for a dial; and the server
// entry hash relects the server entry used for a dial.
//
// These hashes change if the input values change in a way that invalidates
// any stored dial parameters.
func getDialStateHashes(
	config *Config,
	p parameters.ParametersAccessor,
	serverEntry *protocol.ServerEntry) ([]byte, []byte) {

	// MD5 hash is used solely as a data checksum and not for any security
	// purpose.
	hash := md5.New()

	// Add a hash of relevant dial parameter config fields. Config fields
	// that change due to user preference changes, such as selected egress
	// region, are not to be included in config.dialParametersHash.
	//
	// Limitation: the config hash may change even when tactics will override the
	// changed config field.
	hash.Write(config.dialParametersHash)

	// Add the active tactics tag.
	hash.Write([]byte(p.Tag()))

	clientStateHash := hash.Sum(nil)

	hash = md5.New()

	// Add the server entry version and local timestamp, both of which should
	// change when the server entry contents change and/or a new local copy is
	// imported.
	// TODO: marshal entire server entry?
	var serverEntryConfigurationVersion [8]byte
	binary.BigEndian.PutUint64(
		serverEntryConfigurationVersion[:],
		uint64(serverEntry.ConfigurationVersion))
	hash.Write(serverEntryConfigurationVersion[:])
	hash.Write([]byte(serverEntry.LocalTimestamp))

	serverEntryHash := hash.Sum(nil)

	return clientStateHash, serverEntryHash
}

func selectFrontingParameters(
	serverEntry *protocol.ServerEntry) (string, string, error) {

	frontingDialHost := ""
	frontingHost := ""

	if len(serverEntry.MeekFrontingAddressesRegex) > 0 {

		// Generate a front address based on the regex.

		var err error
		frontingDialHost, err = regen.GenerateString(serverEntry.MeekFrontingAddressesRegex)
		if err != nil {
			return "", "", errors.Trace(err)
		}

	} else {

		// Randomly select, for this connection attempt, one front address for
		// fronting-capable servers.

		if len(serverEntry.MeekFrontingAddresses) == 0 {
			return "", "", errors.TraceNew("MeekFrontingAddresses is empty")
		}

		index := prng.Intn(len(serverEntry.MeekFrontingAddresses))
		frontingDialHost = serverEntry.MeekFrontingAddresses[index]
	}

	if len(serverEntry.MeekFrontingHosts) > 0 {

		index := prng.Intn(len(serverEntry.MeekFrontingHosts))
		frontingHost = serverEntry.MeekFrontingHosts[index]

	} else {

		// Backwards compatibility case
		frontingHost = serverEntry.MeekFrontingHost
	}

	return frontingDialHost, frontingHost, nil
}

func selectQUICVersion(
	isFronted bool,
	serverEntry *protocol.ServerEntry,
	p parameters.ParametersAccessor) string {

	limitQUICVersions := p.QUICVersions(parameters.LimitQUICVersions)

	var disableQUICVersions protocol.QUICVersions

	if isFronted {
		if serverEntry.FrontingProviderID == "" {
			// Legacy server entry case
			disableQUICVersions = protocol.QUICVersions{
				protocol.QUIC_VERSION_V1,
				protocol.QUIC_VERSION_RANDOMIZED_V1,
				protocol.QUIC_VERSION_OBFUSCATED_V1,
				protocol.QUIC_VERSION_DECOY_V1,
			}
		} else {
			disableQUICVersions = p.LabeledQUICVersions(
				parameters.DisableFrontingProviderQUICVersions,
				serverEntry.FrontingProviderID)
		}
	}

	quicVersions := make([]string, 0)

	// Don't use gQUIC versions when the server entry specifies QUICv1-only.
	supportedQUICVersions := protocol.SupportedQUICVersions
	if serverEntry.SupportsOnlyQUICv1() {
		supportedQUICVersions = protocol.SupportedQUICv1Versions
	}

	for _, quicVersion := range supportedQUICVersions {

		if len(limitQUICVersions) > 0 &&
			!common.Contains(limitQUICVersions, quicVersion) {
			continue
		}

		// Both tactics and the server entry can specify LimitQUICVersions. In
		// tactics, the parameter is intended to direct certain clients to
		// use a successful protocol variant. In the server entry, the
		// parameter may be used to direct all clients to send
		// consistent-looking protocol variants to a particular server; e.g.,
		// only regular QUIC, or only obfuscated QUIC.
		//
		// The isFronted/QUICVersionIsObfuscated logic predates
		// ServerEntry.LimitQUICVersions; ServerEntry.LimitQUICVersions could
		// now be used to achieve a similar outcome.
		if len(serverEntry.LimitQUICVersions) > 0 &&
			!common.Contains(serverEntry.LimitQUICVersions, quicVersion) {
			continue
		}

		if isFronted &&
			protocol.QUICVersionIsObfuscated(quicVersion) {
			continue
		}

		if common.Contains(disableQUICVersions, quicVersion) {
			continue
		}

		quicVersions = append(quicVersions, quicVersion)
	}

	if len(quicVersions) == 0 {
		return ""
	}

	choice := prng.Intn(len(quicVersions))

	return quicVersions[choice]
}

// selectUserAgentIfUnset selects a User-Agent header if one is not set.
func selectUserAgentIfUnset(
	p parameters.ParametersAccessor, headers http.Header) (bool, string) {

	if _, ok := headers["User-Agent"]; !ok {

		userAgent := ""
		if p.WeightedCoinFlip(parameters.PickUserAgentProbability) {
			userAgent = values.GetUserAgent()
		}

		return true, userAgent
	}

	return false, ""
}

func makeDialCustomHeaders(
	config *Config,
	p parameters.ParametersAccessor) http.Header {

	dialCustomHeaders := make(http.Header)
	if config.CustomHeaders != nil {
		for k, v := range config.CustomHeaders {
			dialCustomHeaders[k] = make([]string, len(v))
			copy(dialCustomHeaders[k], v)
		}
	}

	additionalCustomHeaders := p.HTTPHeaders(parameters.AdditionalCustomHeaders)
	for k, v := range additionalCustomHeaders {
		dialCustomHeaders[k] = make([]string, len(v))
		copy(dialCustomHeaders[k], v)
	}
	return dialCustomHeaders
}

func selectHostName(
	tunnelProtocol string, p parameters.ParametersAccessor) string {

	limitProtocols := p.TunnelProtocols(parameters.CustomHostNameLimitProtocols)
	if len(limitProtocols) > 0 && !common.Contains(limitProtocols, tunnelProtocol) {
		return values.GetHostName()
	}

	if !p.WeightedCoinFlip(parameters.CustomHostNameProbability) {
		return values.GetHostName()
	}

	regexStrings := p.RegexStrings(parameters.CustomHostNameRegexes)
	if len(regexStrings) == 0 {
		return values.GetHostName()
	}

	choice := prng.Intn(len(regexStrings))
	hostName, err := regen.GenerateString(regexStrings[choice])
	if err != nil {
		NoticeWarning("selectHostName: regen.Generate failed: %v", errors.Trace(err))
		return values.GetHostName()
	}

	return hostName
}

// makeHTTPTransformerParameters generates HTTPTransformerParameters using the
// input tactics parameters and optional frontingProviderID context.
func makeHTTPTransformerParameters(p parameters.ParametersAccessor,
	frontingProviderID string, isFronted bool) (*transforms.HTTPTransformerParameters, error) {

	params := transforms.HTTPTransformerParameters{}

	// Select an HTTP transform. If the request is fronted, HTTP request
	// transforms are "scoped" by fronting provider ID. Otherwise, a transform
	// from the default scope (transforms.SCOPE_ANY == "") is selected.

	var specsKey string
	var scopedSpecsNamesKey string

	useTransform := false
	scope := transforms.SCOPE_ANY

	if isFronted {
		if p.WeightedCoinFlip(parameters.FrontedHTTPProtocolTransformProbability) {
			useTransform = true
			scope = frontingProviderID
			specsKey = parameters.FrontedHTTPProtocolTransformSpecs
			scopedSpecsNamesKey = parameters.FrontedHTTPProtocolTransformScopedSpecNames
		}
	} else {
		// unfronted
		if p.WeightedCoinFlip(parameters.DirectHTTPProtocolTransformProbability) {
			useTransform = true
			specsKey = parameters.DirectHTTPProtocolTransformSpecs
			scopedSpecsNamesKey = parameters.DirectHTTPProtocolTransformScopedSpecNames
		}
	}

	if useTransform {

		specs := p.ProtocolTransformSpecs(
			specsKey)
		scopedSpecNames := p.ProtocolTransformScopedSpecNames(
			scopedSpecsNamesKey)

		name, spec := specs.Select(scope, scopedSpecNames)

		if spec != nil {
			params.ProtocolTransformName = name
			params.ProtocolTransformSpec = spec
			var err error
			// transform seed generated
			params.ProtocolTransformSeed, err = prng.NewSeed()
			if err != nil {
				return nil, errors.Trace(err)
			}
		}
	}

	return &params, nil
}

// makeSeedTransformerParameters generates ObfuscatorSeedTransformerParameters
// using the input tactics parameters.
func makeSeedTransformerParameters(p parameters.ParametersAccessor,
	probabilityFieldName, specsKey, scopedSpecsKey string) (*transforms.ObfuscatorSeedTransformerParameters, error) {

	if !p.WeightedCoinFlip(probabilityFieldName) {
		return &transforms.ObfuscatorSeedTransformerParameters{}, nil
	}

	seed, err := prng.NewSeed()
	if err != nil {
		return nil, errors.Trace(err)
	}

	specs := p.ProtocolTransformSpecs(specsKey)
	scopedSpecNames := p.ProtocolTransformScopedSpecNames(scopedSpecsKey)

	name, spec := specs.Select(transforms.SCOPE_ANY, scopedSpecNames)

	if spec == nil {
		return &transforms.ObfuscatorSeedTransformerParameters{}, nil
	} else {
		return &transforms.ObfuscatorSeedTransformerParameters{
			TransformName: name,
			TransformSpec: spec,
			TransformSeed: seed,
		}, nil
	}
}

func makeOSSHPrefixSpecParameters(
	p parameters.ParametersAccessor,
	dialPortNumber string) (*obfuscator.OSSHPrefixSpec, error) {

	if !p.WeightedCoinFlip(parameters.OSSHPrefixProbability) {
		return &obfuscator.OSSHPrefixSpec{}, nil
	}

	specs := p.ProtocolTransformSpecs(parameters.OSSHPrefixSpecs)
	scopedSpecNames := p.ProtocolTransformScopedSpecNames(parameters.OSSHPrefixScopedSpecNames)

	name, spec := specs.Select(dialPortNumber, scopedSpecNames)

	if spec == nil {
		return &obfuscator.OSSHPrefixSpec{}, nil
	} else {
		seed, err := prng.NewSeed()
		if err != nil {
			return nil, errors.Trace(err)
		}
		return &obfuscator.OSSHPrefixSpec{
			Name: name,
			Spec: spec,
			Seed: seed,
		}, nil
	}
}

func makeOSSHPrefixSplitConfig(p parameters.ParametersAccessor) (*obfuscator.OSSHPrefixSplitConfig, error) {

	minDelay := p.Duration(parameters.OSSHPrefixSplitMinDelay)
	maxDelay := p.Duration(parameters.OSSHPrefixSplitMaxDelay)

	seed, err := prng.NewSeed()
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &obfuscator.OSSHPrefixSplitConfig{
		Seed:     seed,
		MinDelay: minDelay,
		MaxDelay: maxDelay,
	}, nil
}

func selectConjureTransport(
	p parameters.ParametersAccessor) string {

	limitConjureTransports := p.ConjureTransports(parameters.ConjureLimitTransports)

	transports := make([]string, 0)

	for _, transport := range protocol.SupportedConjureTransports {

		if len(limitConjureTransports) > 0 &&
			!common.Contains(limitConjureTransports, transport) {
			continue
		}

		transports = append(transports, transport)
	}

	if len(transports) == 0 {
		return ""
	}

	choice := prng.Intn(len(transports))

	return transports[choice]
}
