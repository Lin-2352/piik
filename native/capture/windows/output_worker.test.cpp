// Exercise the production worker with a synthetic codec at its existing link
// boundary. WARP supplies only the device-status API; no screen or audio is used.
#define wmain capture_main
#include "main.cpp"
#undef wmain

#include <cassert>
#include <future>

namespace piik::capture::windows {
class AdaptiveEncoder::Impl final {
 public:
  explicit Impl(Factory create, VideoProfile profile) : encoder(create(profile)) {}
  std::unique_ptr<VideoEncoder> encoder;
};

AdaptiveEncoder::AdaptiveEncoder(OutputKind, VideoProfile profile, ID3D11Device*,
    Factory create, std::unique_ptr<VideoEncoder>, int)
    : impl_(std::make_unique<Impl>(std::move(create), profile)) {}
AdaptiveEncoder::~AdaptiveEncoder() = default;

std::optional<AdaptiveAccessUnit> AdaptiveEncoder::Encode(FrameProducer,
    UINT32 width, UINT32 height, UINT64 timestamp, bool key, UINT32 bitrate,
    EncoderClock::time_point deadline) {
  impl_->encoder->SetBitrate(bitrate);
  return AdaptiveAccessUnit{impl_->encoder->Encode(nullptr, timestamp, key, deadline),
                            width, height, 333'333};
}
}  // namespace piik::capture::windows

class FixtureEncoder final : public VideoEncoder {
 public:
  FixtureEncoder(std::promise<void>* entered, std::shared_future<void> released)
      : VideoEncoder(OutputKind::h264, "fixture", "fixture"),
        entered_(entered), released_(std::move(released)) {}

  EncodedAccessUnit Encode(ID3D11Texture2D*, UINT64 timestamp, bool key,
      EncoderClock::time_point) override {
    if (entered_) {
      entered_->set_value();
      released_.wait();
      throw GateFailure("fixture-encode", "synthetic encoder failed", E_FAIL);
    }
    return {timestamp, key, {1, 2, 3}};
  }
  void SetBitrate(UINT32) override {}

 private:
  std::promise<void>* entered_;
  std::shared_future<void> released_;
};

void CheckWorkerFailureGeneration(ID3D11Device* device, bool replace) {
  std::promise<void> entered, release, output, failure;
  auto began = entered.get_future();
  auto released = release.get_future().share();
  auto delivered = output.get_future();
  auto failed = failure.get_future();
  std::atomic<unsigned> created = 0;
  HANDLE sink = CreateFileW(L"NUL", GENERIC_WRITE, 0, nullptr, OPEN_EXISTING, 0, nullptr);
  assert(sink != INVALID_HANDLE_VALUE);
  ProtocolWriter writer(sink);
  OutputWorker worker(0, VideoProfile{}, OutputKind::h264, device, writer,
      [&](const VideoProfile&) -> std::unique_ptr<VideoEncoder> {
        return std::make_unique<FixtureEncoder>(created++ == 0 ? &entered : nullptr, released);
      }, nullptr, true, [&](const EncodedAccessUnit&) { output.set_value(); },
      [&](std::exception_ptr error) {
        try { std::rethrow_exception(error); }
        catch (const GateFailure& actual) { assert(actual.result() == E_FAIL); }
        failure.set_value();
      });
  auto input = std::make_shared<CaptureInput>();
  input->width = 1280;
  input->height = 720;
  input->timestamp = 333'333;
  worker.Submit(input);
  assert(began.wait_for(std::chrono::seconds(2)) == std::future_status::ready);
  if (replace) {
    worker.SetActive(false);
    assert(worker.SetActive(true));
    worker.Submit(input);
  }
  release.set_value();
  if (replace) {
    assert(delivered.wait_for(std::chrono::seconds(2)) == std::future_status::ready);
    assert(failed.wait_for(std::chrono::milliseconds(0)) == std::future_status::timeout);
    assert(created == 2);
  } else {
    assert(failed.wait_for(std::chrono::seconds(2)) == std::future_status::ready);
    assert(delivered.wait_for(std::chrono::milliseconds(0)) == std::future_status::timeout);
    assert(created == 1);
    assert(!worker.SetActive(false) && !worker.SetActive(true));
  }
  worker.Stop();
  worker.Join();
  CloseHandle(sink);
}

void CheckPrimaryFailureDetails() {
  std::ostringstream output;
  auto previous = std::cerr.rdbuf(output.rdbuf());
  assert(OutputFailureDetail(3, std::make_exception_ptr(
      GateFailure("fixture-output", "original cause", E_ACCESSDENIED))) == "fixture-output");
  std::cerr.rdbuf(previous);
  const auto text = output.str();
  assert(text.find("layer=3") != std::string::npos);
  assert(text.find("original cause") != std::string::npos);
  assert(text.find("hresult=0x80070005") != std::string::npos);
  // With no COM apartment, the real decoder fails at its first API boundary.
  bool rejected = false;
  try { piik::capture::H264Decoder decoder(nullptr); }
  catch (const GateFailure& error) {
    rejected = error.stage() == "h264-decoder-create" && error.result() == CO_E_NOTINITIALIZED;
  }
  assert(rejected);
}

void CheckH264ProfileCompatibility() {
  const VideoProfile profile;
  for (const auto* value : {"42c01f", "42e01f", "42E01F", "4d801f", "58c01f"})
    assert(profile.accepts_h264_profile_level_id(value));
  for (const auto* value : {"42001f", "4d001f", "64001f", "42e11f", "42e01", "42e01fg",
                           "42e0gg", "42e034", "42e01e", " 42c01"})
    assert(!profile.accepts_h264_profile_level_id(value));

  HANDLE read = nullptr, write = nullptr;
  assert(CreatePipe(&read, &write, nullptr, 4096));
  ProtocolWriter writer(write);
  ProductArguments arguments;
  arguments.outputs = {profile};
  EncodedAccessUnit output{0, true, {0, 0, 0, 1, 0x67, 0x42, 0xe0, 0x1f}};
  WriteVideoActive(writer, arguments, OutputKind::h264, output);
  std::array<char, 4096> bytes{};
  DWORD count = 0;
  assert(ReadFile(read, bytes.data(), static_cast<DWORD>(bytes.size()), &count, nullptr));
  const std::string status(bytes.data(), count);
  assert(status.find("\"profileLevelId\":\"42e01f\"") != std::string::npos);
  assert(status.find("42c01f") == std::string::npos);
  // AV1 reports its codec and carries no H264 profile-level-id.
  WriteVideoActive(writer, arguments, OutputKind::av1, output);
  count = 0;
  assert(ReadFile(read, bytes.data(), static_cast<DWORD>(bytes.size()), &count, nullptr));
  const std::string av1(bytes.data(), count);
  assert(av1.find("\"codec\":\"av1\"") != std::string::npos &&
         av1.find("\"hardwareOnly\":true") != std::string::npos &&
         av1.find("profileLevelId") == std::string::npos);
  WriteVideoActive(writer, arguments, OutputKind::h265, output);
  count = 0;
  assert(ReadFile(read, bytes.data(), static_cast<DWORD>(bytes.size()), &count, nullptr));
  const std::string hevc(bytes.data(), count);
  assert(hevc.find("\"codec\":\"h265\"") != std::string::npos &&
         hevc.find("\"hardwareOnly\":true") != std::string::npos &&
         hevc.find("profileLevelId") == std::string::npos);
  CloseHandle(write);
  CloseHandle(read);
}

// Size-delimited OBUs: temporal delimiter, sequence header, then a frame.
void CheckAv1ObuInspection() {
  const std::vector<UINT8> key{0x12, 0x00, 0x0a, 0x02, 0xaa, 0xbb, 0x32, 0x01, 0xcc};
  const auto summary = InspectObus(key);
  assert(summary.valid && summary.sequence_header && summary.frame);
  const auto delta = InspectObus({0x12, 0x00, 0x32, 0x02, 0x01, 0x02});
  assert(delta.valid && !delta.sequence_header && delta.frame);
  assert(!InspectObus({0x10, 0x00}).valid);              // obu_has_size_field is required
  assert(!InspectObus({0x32, 0x05, 0x01}).valid);        // size exceeds the access unit
  assert(!InspectObus({0x00, 0x00, 0x00, 0x01, 0x67}).valid);  // Annex B is not AV1
  assert(!InspectObus({}).valid);
}

// HEVC Annex B: VPS(32) SPS(33) PPS(34) IDR_W_RADL(19); TRAIL_R(1) is a delta.
void CheckHevcInspection() {
  const std::vector<UINT8> key{0, 0, 0, 1, 0x40, 0x01, 0xaa, 0, 0, 0, 1, 0x42, 0x01, 0xbb,
                               0, 0, 0, 1, 0x44, 0x01, 0xcc, 0, 0, 1, 0x26, 0x01, 0xdd};
  const auto summary = InspectHevc(key);
  assert(summary.annex_b && summary.parameter_sets && summary.irap && summary.picture);
  const auto delta = InspectHevc({0, 0, 0, 1, 0x02, 0x01, 0xee});
  assert(delta.annex_b && !delta.parameter_sets && !delta.irap && delta.picture);
  assert(!InspectHevc({0, 0, 0, 1, 0x40, 0x01}).picture);   // parameter sets alone
  assert(!InspectHevc({0, 0, 0, 1, 0xc0, 0x01}).annex_b);   // forbidden_zero_bit
  assert(!InspectHevc({0x12, 0x00}).annex_b);               // AV1 OBUs are not Annex B
  // Temporal layers: NVENC's enhancement frame is AUD(35) + TSA_N(2), both TID 1.
  assert(summary.temporal_id == 0 && delta.temporal_id == 0);
  const auto enhancement = InspectHevc({0, 0, 0, 1, 0x46, 0x02, 0x50, 0, 0, 0, 1, 0x04, 0x02, 0xee});
  assert(enhancement.picture && !enhancement.irap && enhancement.temporal_id == 1);
}

// NVIDIA's H.264 temporal layers, as captured: a base key frame is AUD, SPS, PPS,
// SVC prefix NAL 14 (temporal_id 0) and IDR; an enhancement frame is AUD, prefix
// (temporal_id 1, nal_ref_idc 0) and a non-reference slice.
void CheckH264TemporalInspection() {
  const auto base = InspectAnnexB({0, 0, 0, 1, 0x09, 0xf0, 0, 0, 0, 1, 0x67, 0x42, 0xc0, 0x34,
                                   0, 0, 0, 1, 0x68, 0xce, 0, 0, 0, 1, 0x6e, 0xc0, 0x80, 0x07,
                                   0, 0, 0, 1, 0x65, 0x88});
  assert(base.annex_b && base.sps && base.pps && base.idr && base.temporal_id == 0);
  const auto enhancement = InspectAnnexB({0, 0, 0, 1, 0x09, 0xf0, 0, 0, 0, 1, 0x0e, 0x40, 0x80, 0x27,
                                          0, 0, 0, 1, 0x01, 0x9a});
  assert(enhancement.annex_b && !enhancement.idr && enhancement.temporal_id == 1);
  // Without a prefix NAL the stream is single-layer.
  assert(InspectAnnexB({0, 0, 0, 1, 0x41, 0x9a}).temporal_id == 0);
}

// A reused encoder's first live frame can be an enhancement frame with no SPS;
// only a key frame (which carries the profile) may announce an active capture.
void CheckActiveWaitsForKeyFrame() {
  assert(!ActivatesCapture(EncodedAccessUnit{1, false, {0, 0, 0, 1, 0x01, 0x9a}}));
  assert(ActivatesCapture(EncodedAccessUnit{2, true, {0, 0, 0, 1, 0x65, 0x88}}));
}

// The quality floor (max quantizer) only applies where the bitrate budget is
// generous enough that it cannot defeat the ceiling. Measured: H.264 at QP 30
// needed 27-35 Mbps on complex 2560x1600@120 content, so pinning QP 30 under a
// 15 Mbps ceiling made the encoder exceed it by 2x and overflow the sender.
void CheckQuantizerFloorHonorsCeiling() {
  constexpr UINT32 kUnbounded = VideoProfile::kQuantizerUnbounded;
  constexpr UINT32 kFloor = VideoProfile::kMaxQuantizer;
  // 15 Mbps over 2560x1600@120 is 0.031 bits/pixel: far below what QP 30 costs.
  assert(MaxQuantizer(OutputKind::h264, VideoProfile{2560, 1600, 120, 15'000'000}) == kUnbounded);
  assert(MaxQuantizer(OutputKind::h265, VideoProfile{2560, 1600, 120, 15'000'000}) == kUnbounded);
  // The derived half-size copy at a quarter of the budget is the same density.
  assert(MaxQuantizer(OutputKind::h264, VideoProfile{1280, 800, 120, 3'750'000}) == kUnbounded);
  // A generous budget keeps the floor (the case it was measured for: 38 Mbps).
  assert(MaxQuantizer(OutputKind::h264, VideoProfile{2560, 1600, 120, 40'000'000}) == kFloor);
  assert(MaxQuantizer(OutputKind::h264, VideoProfile{1920, 1080, 60, 20'000'000}) == kFloor);
  // HEVC is about twice as efficient at one quantizer, so its threshold is lower.
  assert(MaxQuantizer(OutputKind::h265, VideoProfile{2560, 1600, 120, 25'000'000}) == kFloor);
  assert(MaxQuantizer(OutputKind::h264, VideoProfile{2560, 1600, 120, 25'000'000}) == kUnbounded);
  // AV1's scale is different and unmeasured: unchanged.
  assert(MaxQuantizer(OutputKind::av1, VideoProfile{2560, 1600, 120, 15'000'000}) == kFloor);
  // A live bitrate change moves the answer in both directions.
  VideoProfile live{2560, 1600, 120, 40'000'000};
  assert(MaxQuantizer(OutputKind::h264, live) == kFloor);
  live.bit_rate = 12'000'000;
  assert(MaxQuantizer(OutputKind::h264, live) == kUnbounded);
}

// Two temporal layers only where halving still leaves smooth motion (>= 50 fps)
// and the viewer cap can read them (LiveKit parses the HEVC temporal id).
void CheckTemporalLayers() {
  assert(TemporalLayerCount(OutputKind::h265, VideoProfile{2560, 1600, 120}) == 2);
  assert(TemporalLayerCount(OutputKind::h265, VideoProfile{2560, 1600, 100}) == 2);
  assert(TemporalLayerCount(OutputKind::h265, VideoProfile{2560, 1600, 90}) == 1);
  assert(TemporalLayerCount(OutputKind::h265, VideoProfile{2560, 1600, 60}) == 1);
  assert(TemporalLayerCount(OutputKind::h264, VideoProfile{2560, 1600, 120}) == 2);
  assert(TemporalLayerCount(OutputKind::h264, VideoProfile{2560, 1600, 90}) == 1);
  assert(TemporalLayerCount(OutputKind::av1, VideoProfile{2560, 1600, 120}) == 1);
}

void CheckEncoderCandidates() {
  const std::vector<EncoderCandidate> candidates{{0, 0}, {1, 0}, {1, 1}, {2, 0}};
  const EncoderCandidate preferred{1, 0};
  std::vector<EncoderCandidate> tried;
  const auto deadline = EncoderClock::now() + std::chrono::seconds(1);
  std::ostringstream diagnostic;
  auto previous = std::cerr.rdbuf(diagnostic.rdbuf());
  const auto chosen = SelectEncoderCandidate(candidates, preferred, deadline,
      [&](EncoderCandidate candidate) {
        tried.push_back(candidate);
        if (candidate == preferred)
          throw GateFailure("input-texture-create", "fixture device loss", DXGI_ERROR_DEVICE_REMOVED);
        if (candidate != EncoderCandidate{1, 1})
          throw GateFailure("fixture-activation", "candidate is not usable", E_FAIL);
      });
  std::cerr.rdbuf(previous);
  assert((chosen == EncoderCandidate{1, 1}));
  assert((tried == std::vector<EncoderCandidate>{{1, 0}, {0, 0}, {1, 1}}));
  assert(diagnostic.str().find("stage=input-texture-create") != std::string::npos);
  assert(diagnostic.str().find("fixture device loss") != std::string::npos);
  assert(diagnostic.str().find("hresult=0x887a0005") != std::string::npos);
  tried.clear();
  try {
    SelectEncoderCandidate(candidates, chosen, deadline, [&](EncoderCandidate candidate) {
      tried.push_back(candidate);
      throw GateFailure("fixture-all-failed", "no working candidate", E_FAIL);
    });
    assert(false);
  } catch (const GateFailure& error) {
    assert(error.stage() == "fixture-all-failed");
  }
  assert(tried.size() == candidates.size() && tried.front() == chosen);
  tried.clear();
  try {
    SelectEncoderCandidate(candidates, preferred, EncoderClock::now(),
        [&](EncoderCandidate candidate) { tried.push_back(candidate); });
    assert(false);
  } catch (const GateFailure& error) {
    assert(error.stage() == "codec-probe-timeout");
  }
  assert(tried.empty());
}

class CadenceFixtureEncoder final : public VideoEncoder {
 public:
  CadenceFixtureEncoder(std::chrono::milliseconds latency, bool dropped, bool cold_only)
      : VideoEncoder(OutputKind::h264, "cadence-fixture", "cadence-fixture"),
        latency_(latency), dropped_(dropped), cold_only_(cold_only) {}

  EncodedAccessUnit Encode(ID3D11Texture2D*, UINT64 timestamp, bool key,
      EncoderClock::time_point deadline) override {
    if (!cold_only_ || calls_ == 0) std::this_thread::sleep_for(latency_);
    ++calls_;
    RequireEncoderTime(deadline);
    // An empty unit stands for an input the pipeline did not encode.
    return dropped_ ? EncodedAccessUnit{timestamp, key, {}} : EncodedAccessUnit{timestamp, key, {1, 2, 3}};
  }
  void SetBitrate(UINT32) override {}

 private:
  std::chrono::milliseconds latency_;
  bool dropped_;
  bool cold_only_;
  unsigned calls_ = 0;
};

void CheckAutoCadenceProbe(ID3D11Device* device) {
  // The Browser probe allows one 100 ms poll of lag at each target rate.
  assert(CadenceLagFrames(5) == 1 && CadenceLagFrames(15) == 2 &&
         CadenceLagFrames(30) == 3 && CadenceLagFrames(60) == 6);
  VideoProfile profile;
  profile.width = 64;
  profile.height = 64;
  auto probe = [&](std::chrono::milliseconds latency, bool dropped,
                   EncoderClock::time_point deadline, bool cold_only = false) {
    return SustainsEncodedCadence([=](const VideoProfile&) -> std::unique_ptr<VideoEncoder> {
      return std::make_unique<CadenceFixtureEncoder>(latency, dropped, cold_only);
    }, device, profile, deadline);
  };
  const auto deadline = EncoderClock::now() + std::chrono::seconds(30);
  // Valid output at a fraction of the source rate is the driver defect Auto rejects.
  assert(!probe(std::chrono::milliseconds(0), true, deadline));
  // Cold startup must not reject an encoder that sustains the measured cadence.
  assert(probe(std::chrono::milliseconds(220), false, deadline, true));
  // Sustained slow encoding still fails after warmup.
  assert(!probe(std::chrono::milliseconds(300), false, deadline));
  bool expired = false;
  try {
    probe(std::chrono::milliseconds(0), false, EncoderClock::time_point::min());
  } catch (const GateFailure& error) {
    expired = error.stage() == "codec-probe-timeout";
  }
  assert(expired);
}

void CheckOutputSampleAlignment() {
  Runtime runtime;
  for (DWORD alignment : {0u, 1u, 16u, 64u, 512u}) {
    MFT_OUTPUT_STREAM_INFO info{};
    info.cbSize = 4096;
    info.cbAlignment = alignment;
    auto sample = CreateCallerOutputSample(info);
    ComPtr<IMFMediaBuffer> buffer;
    Check(sample->GetBufferByIndex(0, &buffer), "fixture-output-buffer");
    BYTE* data = nullptr;
    DWORD maximum = 0;
    Check(buffer->Lock(&data, &maximum, nullptr), "fixture-output-lock");
    const auto address = reinterpret_cast<uintptr_t>(data);
    Check(buffer->Unlock(), "fixture-output-unlock");
    assert(maximum >= info.cbSize);
    assert(alignment == 0 || address % alignment == 0);
  }
}

void CheckSurfaceSample(ID3D11Device* device) {
  Runtime runtime;
  const VideoProfile profile{64, 64, 30, 3'000'000};
  auto texture = CreateSyntheticTexture(device, 0, profile);
  auto sample = CreateSurfaceSample(texture.Get(), 1'000'000, 333'333);
  ComPtr<IMFMediaBuffer> buffer;
  Check(sample->GetBufferByIndex(0, &buffer), "fixture-sample-buffer");
  DWORD current = 0, maximum = 0, total = 0;
  Check(buffer->GetCurrentLength(&current), "fixture-buffer-length");
  Check(buffer->GetMaxLength(&maximum), "fixture-buffer-capacity");
  Check(sample->GetTotalLength(&total), "fixture-sample-length");
  assert(current == profile.width * profile.height * 3 / 2);
  assert(total == current && maximum == current);
  ComPtr<IMFDXGIBuffer> surface;
  Check(buffer.As(&surface), "fixture-surface-buffer");
  ComPtr<ID3D11Texture2D> retained;
  Check(surface->GetResource(IID_PPV_ARGS(&retained)), "fixture-surface-resource");
  assert(retained.Get() == texture.Get());
  LONGLONG time = 0, duration = 0;
  Check(sample->GetSampleTime(&time), "fixture-sample-time");
  Check(sample->GetSampleDuration(&duration), "fixture-sample-duration");
  assert(time == 1'000'000 && duration == 333'333);
}

int main() {
  CheckTemporalLayers();
  CheckH264TemporalInspection();
  CheckActiveWaitsForKeyFrame();
  CheckQuantizerFloorHonorsCeiling();
  VideoProfile profile{2560, 1440, 60, 12'000'000};
  ValidateVideoProfile(profile);
  assert(profile.h264_level() == 51 && profile.profile_level_id() == "42c033");
  profile = VideoProfile{1920, 1080, 60, 8'000'000};
  assert(profile.h264_level() == 42);
  // MaxBR raises the level as well as macroblock rate does.
  assert((VideoProfile{1920, 1080, 30, 160'000'000}.h264_level() == 51));
  assert((VideoProfile{1280, 720, 30, 20'000'000}.h264_level() == 40));
  assert((VideoProfile{1280, 720, 120, 51'000'000}.h264_level() == 50));
  assert((VideoProfile{2560, 1600, 120, 51'000'000}.h264_level() == 52));
  assert((VideoProfile{1920, 1080, 240, 160'000'000}.h264_level() == 52));
  ProductArguments source;
  source.profile = VideoProfile{2560, 1600, 240, 51'000'000};
  source.outputs = {VideoProfile{1280, 800, 240, 12'750'000}, source.profile};
  ClampToH264Level(source);
  assert(source.profile.frame_rate == 129 && source.outputs[1].frame_rate == 129 &&
         source.outputs[0].frame_rate == 129 && source.profile.h264_level() == 52);
  source.profile = VideoProfile{5120, 2880, 30, 51'000'000};
  try {
    ClampToH264Level(source);
    assert(false);
  } catch (const GateFailure& error) {
    assert(error.stage() == "profile-level");
  }
  profile = VideoProfile{3840, 2160, 30, 12'000'000};
  try {
    ValidateVideoProfile(profile);
    assert(false);
  } catch (const GateFailure& error) {
    assert(error.stage() == "argument-profile");
  }
  CheckEncoderCandidates();
  CheckH264ProfileCompatibility();
  CheckAv1ObuInspection();
  CheckHevcInspection();
  CheckPrimaryFailureDetails();
  CheckOutputSampleAlignment();
  ComPtr<ID3D11Device> device;
  Check(D3D11CreateDevice(nullptr, D3D_DRIVER_TYPE_WARP, nullptr, 0, nullptr, 0,
                         D3D11_SDK_VERSION, &device, nullptr, nullptr), "fixture-warp");
  CheckSurfaceSample(device.Get());
  CheckAutoCadenceProbe(device.Get());
  CheckWorkerFailureGeneration(device.Get(), false);
  CheckWorkerFailureGeneration(device.Get(), true);
}
