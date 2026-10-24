#pragma once

#ifndef NOMINMAX
#define NOMINMAX
#endif
#include <windows.h>

#include <codecapi.h>
#include <d3d11.h>
#include <dxgi1_2.h>
#include <strmif.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mftransform.h>
#include <wrl/client.h>

#include "capture_error.h"
#include "capture_output.h"
#include "mft_event_reader.h"

#include <chrono>
#include <iomanip>
#include <optional>
#include <sstream>
#include <stdexcept>
#include <string>
#include <utility>
#include <vector>

namespace piik::capture::windows {

using Microsoft::WRL::ComPtr;

enum class DegradationPreference {
  resolution,
  balanced,
  framerate,
};

constexpr UINT64 kH264MaxFrameMacroblocks = 36'864;
constexpr UINT64 kH264MaxMacroblocksPerSecond = 2'073'600;

struct VideoProfile final {
  UINT32 width = 1280;
  UINT32 height = 720;
  UINT32 frame_rate = 30;
  UINT32 bit_rate = 3'000'000;
  DegradationPreference preference = DegradationPreference::balanced;
  // Constant bitrate: CBR with a tight buffer. Otherwise variable: follows the
  // picture up to the target. Appended last; tests aggregate-initialise this.
  bool constant_bitrate = false;

  UINT32 gop_frames() const { return frame_rate * 2; }
  // Both modes aim for the full target. Variable (peak-constrained VBR) may still
  // spend less on easy frames, but is never told to aim lower: a lowered mean
  // raised the quantiser on ordinary content and made fine detail shimmer.
  UINT32 mean_bit_rate() const { return bit_rate; }
  static constexpr UINT32 kMaxQuantizer = 30;
  // How often the capture loop re-reads display presentation (window resize).
  UINT32 presentation_refresh_frames() const { return frame_rate * 2; }
  LONGLONG frame_duration_100ns() const {
    return 10'000'000 / static_cast<LONGLONG>(frame_rate);
  }
  // The hardware encoder's preset is a latency choice: its slowest, highest
  // quality mode takes tens of milliseconds a frame at high pixel rates and
  // cannot hold the requested frame rate, which stutters worse than a lower
  // preset. Quality is therefore only preferred while the frame budget allows.
  UINT32 quality_vs_speed() const {
    const UINT64 pixel_rate = static_cast<UINT64>(width) * height * frame_rate;
    if (preference == DegradationPreference::framerate) return 0;
    // Above 300 Mpx/s (Source 2560x1600 at 120) the middle preset still encodes
    // in 4-6 ms of the 8.3 ms frame and keeps visibly more detail than the fastest.
    if (pixel_rate > 300'000'000ull) return 50;
    if (preference == DegradationPreference::resolution && pixel_rate <= 150'000'000ull) return 100;
    return 50;
  }
  UINT64 macroblocks() const {
    return ((static_cast<UINT64>(width) + 15) / 16) *
           ((static_cast<UINT64>(height) + 15) / 16);
  }
  // Level 5.2 is the highest H264 level WebRTC negotiates. Its rate is a
  // ceiling: callers lower the frame rate to it before choosing a level.
  UINT32 h264_max_frame_rate() const {
    return static_cast<UINT32>(kH264MaxMacroblocksPerSecond / macroblocks());
  }
  // H.264 Table A-1 limits, with Constrained Baseline VCL MaxBR in kbit/s.
  UINT32 h264_level() const {
    const UINT64 frame = macroblocks();
    const UINT64 rate = frame * frame_rate;
    const auto fits = [&](UINT64 max_frame, UINT64 max_rate, UINT64 max_kbps) {
      return frame <= max_frame && rate <= max_rate && bit_rate <= max_kbps * 1'000;
    };
    if (fits(3'600, 108'000, 14'000)) return 31;
    if (fits(8'192, 245'760, 20'000)) return 40;
    if (fits(8'704, 522'240, 50'000)) return 42;
    if (fits(22'080, 589'824, 135'000)) return 50;
    if (fits(kH264MaxFrameMacroblocks, 983'040, 240'000)) return 51;
    return 52;
  }
  std::string profile_level_id() const {
    std::ostringstream output;
    output << "42c0" << std::hex << std::setfill('0') << std::setw(2)
           << h264_level();
    return output.str();
  }
  bool accepts_h264_profile_level_id(const std::string& value) const;
};

constexpr VideoProfile kDefaultVideoProfile{};

struct Runtime final {
  Runtime() {
    Check(CoInitializeEx(nullptr, COINIT_MULTITHREADED), "com-initialize");
    com_started = true;
    Check(MFStartup(MF_VERSION, MFSTARTUP_FULL), "mf-startup");
    mf_started = true;
  }

  ~Runtime() {
    if (mf_started) {
      MFShutdown();
    }
    if (com_started) {
      CoUninitialize();
    }
  }

  bool com_started = false;
  bool mf_started = false;
};

struct Adapter final {
  UINT index = 0;
  DXGI_ADAPTER_DESC1 description = {};
  ComPtr<IDXGIAdapter1> value;
};

struct DeviceContext final {
  ComPtr<ID3D11Device> device;
  ComPtr<ID3D11DeviceContext> context;
  ComPtr<IMFDXGIDeviceManager> manager;
  D3D_FEATURE_LEVEL feature_level = D3D_FEATURE_LEVEL_9_1;
};

class ActivationList final {
 public:
  ActivationList() = default;
  ActivationList(const ActivationList&) = delete;
  ActivationList& operator=(const ActivationList&) = delete;
  ActivationList(ActivationList&& other) noexcept
      : values(std::exchange(other.values, nullptr)),
        count(std::exchange(other.count, 0)) {}

  ~ActivationList() {
    for (UINT32 index = 0; index < count; ++index) {
      values[index]->Release();
    }
    CoTaskMemFree(values);
  }

  IMFActivate** values = nullptr;
  UINT32 count = 0;
};

struct SelectedTransform final {
  SelectedTransform() = default;
  SelectedTransform(const SelectedTransform&) = delete;
  SelectedTransform& operator=(const SelectedTransform&) = delete;
  SelectedTransform(SelectedTransform&&) noexcept = default;
  SelectedTransform& operator=(SelectedTransform&&) = delete;
  ~SelectedTransform() { if (activation) activation->ShutdownObject(); }

  std::string name;
  std::string clsid;
  ComPtr<IMFActivate> activation;
  ComPtr<IMFTransform> transform;
  ComPtr<ICodecAPI> codec;
  ComPtr<IMFMediaEventGenerator> events;
};

struct NalSummary final {
  bool annex_b = false;
  bool sps = false;
  bool pps = false;
  bool idr = false;
  UINT8 temporal_id = 0;  // from an SVC prefix NAL (14); 0 for a single-layer stream
  std::optional<std::string> profile_level_id;
};

// AV1 low-overhead bitstream (every OBU carries its size), as RTP packetizes it.
// HEVC Annex B (H.265 7.4.2.2): 6-bit NAL types; IRAP pictures are 16-21.
struct HevcSummary final {
  bool annex_b = false;
  bool parameter_sets = false;  // VPS, SPS and PPS all present
  bool irap = false;
  bool picture = false;
  UINT8 temporal_id = 0;  // highest nuh_temporal_id of the picture's slices
};

struct ObuSummary final {
  bool valid = false;
  bool sequence_header = false;
  bool frame = false;
};

// Hardware encoders this process drives through one Media Foundation path.
inline const GUID& EncoderSubtype(OutputKind kind) {
  return kind == OutputKind::av1 ? MFVideoFormat_AV1
       : kind == OutputKind::h265 ? MFVideoFormat_HEVC : MFVideoFormat_H264;
}
inline const char* CodecName(OutputKind kind) {
  return kind == OutputKind::av1 ? "av1" : kind == OutputKind::h265 ? "h265"
       : kind == OutputKind::h264 ? "h264" : "vp8";
}
// AV1 and HEVC are single-layer hardware streams with no Piik level contract.
inline bool IsNewHardwareCodec(OutputKind kind) {
  return kind == OutputKind::av1 || kind == OutputKind::h265;
}

std::vector<Adapter> EnumerateAdapters();
const Adapter& SelectAdapter(const std::vector<Adapter>& adapters, UINT index);
DeviceContext CreateDevice(const Adapter& adapter);
ActivationList EnumerateHardwareEncoders(const Adapter& adapter,
                                         bool require_candidate = true,
                                         const GUID& format = MFVideoFormat_H264);
SelectedTransform ActivateTransform(const ActivationList& list, UINT index,
                                    IMFDXGIDeviceManager* manager);
std::wstring GetOptionalString(IMFAttributes* attributes, const GUID& key);
std::string NarrowAscii(const std::wstring& value);
std::string GuidString(const GUID& value);

// The hardware fixture uses the same configuration and output validation.
void SetU32(ICodecAPI* codec, const GUID& key, UINT32 expected,
            const std::string& name);
void ConfigureCodec(ICodecAPI* codec,
                    const VideoProfile& profile = kDefaultVideoProfile);
ComPtr<IMFMediaType> CreateOutputType(
    const VideoProfile& profile = kDefaultVideoProfile, OutputKind kind = OutputKind::h264);
ComPtr<IMFMediaType> CreateInputType(
    const VideoProfile& profile = kDefaultVideoProfile, OutputKind kind = OutputKind::h264);
void ValidateMediaTypes(IMFTransform* transform,
                        const VideoProfile& profile = kDefaultVideoProfile,
                        OutputKind kind = OutputKind::h264);
void ValidateCodecReadback(ICodecAPI* codec,
                           const VideoProfile& profile = kDefaultVideoProfile);
void ForceKeyFrame(ICodecAPI* codec);
ComPtr<IMFSample> CreateCallerOutputSample(const MFT_OUTPUT_STREAM_INFO& info);
ComPtr<IMFSample> PullOutput(IMFTransform* transform,
                            const MFT_OUTPUT_STREAM_INFO& info);
ComPtr<IMFSample> CreateSurfaceSample(ID3D11Texture2D* texture,
                                      LONGLONG timestamp100ns,
                                      LONGLONG duration100ns);
NalSummary InspectAnnexB(const std::vector<UINT8>& bytes);
ObuSummary InspectObus(const std::vector<UINT8>& bytes);
HevcSummary InspectHevc(const std::vector<UINT8>& bytes);
// Two temporal layers let the Host send a viewer that cannot decode the full
// rate every other frame. HEVC carries the layer in its NAL header and H.264 in
// an SVC prefix NAL; only a rate whose half stays smooth (>= 50 fps) is split.
UINT32 TemporalLayerCount(OutputKind kind, const VideoProfile& profile);
std::vector<UINT8> ReadSample(IMFSample* sample);

struct EncodedAccessUnit final {
  UINT64 timestamp100ns = 0;
  bool key_frame = false;
  std::vector<UINT8> bytes;
};

using EncoderClock = std::chrono::steady_clock;

void RequireEncoderTime(EncoderClock::time_point deadline);

class VideoEncoder {
 public:
  VideoEncoder(OutputKind kind, std::string name, std::string identity)
      : kind(kind), name(std::move(name)), identity(std::move(identity)) {}
  virtual ~VideoEncoder() = default;
  virtual EncodedAccessUnit Encode(ID3D11Texture2D* texture,
                                    UINT64 timestamp100ns,
                                    bool force_key_frame,
                                    EncoderClock::time_point deadline =
                                        EncoderClock::time_point::max()) = 0;
  virtual void SetBitrate(UINT32 bitrate) = 0;
  const OutputKind kind;
  const std::string name;
  const std::string identity;
};

// The caller owns COM/MF startup and serializes access to each encoder.
// One hardware Media Foundation encoder: H264 (Annex B) or AV1 (OBUs).
class LiveEncoder final : public VideoEncoder {
 public:
  explicit LiveEncoder(SelectedTransform selected,
                       VideoProfile profile = kDefaultVideoProfile,
                       OutputKind kind = OutputKind::h264);
  ~LiveEncoder() override;
  LiveEncoder(const LiveEncoder&) = delete;
  LiveEncoder& operator=(const LiveEncoder&) = delete;

  void SetBitrate(UINT32 bitrate) override;
  EncodedAccessUnit Encode(
      ID3D11Texture2D* texture, UINT64 timestamp100ns, bool force_key_frame,
      EncoderClock::time_point deadline = EncoderClock::time_point::max()) override;

 private:
  MediaEventType NextEvent(EncoderClock::time_point deadline, const char* timeout_stage);
  void WaitForInput(EncoderClock::time_point deadline);

  SelectedTransform selected_;
  MftEventReader events_;
  VideoProfile profile_;
  // The SPS level is fixed by the session's configured profile; live bitrate
  // changes must not move the level the bitstream is checked against.
  const VideoProfile configured_;
  MFT_OUTPUT_STREAM_INFO output_info_ = {};
  UINT32 input_requests_ = 0;
  std::optional<std::string> profile_level_id_;
  bool sequence_header_seen_ = false;
  bool started_ = false;
};

}  // namespace piik::capture::windows
