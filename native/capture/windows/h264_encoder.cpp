#include "h264_encoder.h"

#include "capture_output.h"
#include "capture_color.h"

#include <d3d10_1.h>
#include <evr.h>
#include <mferror.h>
#include <propvarutil.h>

#include <algorithm>
#include <charconv>

namespace piik::capture::windows {

namespace {
constexpr DWORD kMaxEncodedSampleBytes = 4 * 1024 * 1024;
}

bool VideoProfile::accepts_h264_profile_level_id(const std::string& value) const {
  if (value.size() != 6) return false;
  UINT32 parsed = 0;
  const auto end = value.data() + value.size();
  const auto result = std::from_chars(value.data(), end, parsed, 16);
  if (result.ec != std::errc{} || result.ptr != end || (parsed & 0xff) != h264_level()) return false;
  const auto idc = parsed >> 16;
  const auto constraints = (parsed >> 8) & 0xff;
  // RFC 6184 Table 5: these are the same Constrained Baseline tools, not
  // permission to emit Main/High. Extra constraint flags need not be identical.
  return (idc == 0x42 && (constraints & 0x4f) == 0x40) ||
         (idc == 0x4d && (constraints & 0x8f) == 0x80) ||
         (idc == 0x58 && (constraints & 0xcf) == 0xc0);
}

std::string NarrowAscii(const std::wstring& value) {
  std::string result;
  result.reserve(value.size());
  for (wchar_t character : value) {
    result.push_back(character >= 32 && character <= 126
                         ? static_cast<char>(character)
                         : '?');
  }
  return result;
}

std::string GuidString(const GUID& value) {
  wchar_t buffer[40] = {};
  if (StringFromGUID2(value, buffer, static_cast<int>(std::size(buffer))) == 0) {
    return "unavailable";
  }
  return NarrowAscii(buffer);
}

std::string HexByte(UINT8 value) {
  std::ostringstream output;
  output << std::hex << std::setfill('0') << std::setw(2)
         << static_cast<UINT32>(value);
  return output.str();
}

std::string ProfileLevelId(UINT8 profile, UINT8 constraints, UINT8 level) {
  return HexByte(profile) + HexByte(constraints) + HexByte(level);
}

std::vector<Adapter> EnumerateAdapters() {
  ComPtr<IDXGIFactory1> factory;
  Check(CreateDXGIFactory1(IID_PPV_ARGS(&factory)), "dxgi-factory");

  std::vector<Adapter> adapters;
  for (UINT index = 0;; ++index) {
    ComPtr<IDXGIAdapter1> adapter;
    HRESULT result = factory->EnumAdapters1(index, &adapter);
    if (result == DXGI_ERROR_NOT_FOUND) {
      break;
    }
    Check(result, "dxgi-enumerate-adapter");

    Adapter current;
    current.index = index;
    current.value = adapter;
    Check(adapter->GetDesc1(&current.description), "dxgi-adapter-description");
    if ((current.description.Flags & DXGI_ADAPTER_FLAG_SOFTWARE) == 0) {
      adapters.push_back(std::move(current));
    }
  }
  return adapters;
}

const Adapter& SelectAdapter(const std::vector<Adapter>& adapters, UINT index) {
  auto found = std::find_if(adapters.begin(), adapters.end(),
                            [index](const Adapter& adapter) {
                              return adapter.index == index;
                            });
  if (found == adapters.end()) {
    Fail("adapter-select", "selected DXGI adapter index is unavailable");
  }
  return *found;
}

DeviceContext CreateDevice(const Adapter& adapter) {
  DeviceContext result;
  constexpr D3D_FEATURE_LEVEL levels[] = {
      D3D_FEATURE_LEVEL_12_1, D3D_FEATURE_LEVEL_12_0,
      D3D_FEATURE_LEVEL_11_1, D3D_FEATURE_LEVEL_11_0,
  };
  Check(D3D11CreateDevice(adapter.value.Get(), D3D_DRIVER_TYPE_UNKNOWN, nullptr,
                          D3D11_CREATE_DEVICE_VIDEO_SUPPORT |
                              D3D11_CREATE_DEVICE_BGRA_SUPPORT,
                          levels, static_cast<UINT>(std::size(levels)),
                          D3D11_SDK_VERSION, &result.device,
                          &result.feature_level, &result.context),
        "d3d11-create-device");

  ComPtr<ID3D10Multithread> multithread;
  Check(result.device.As(&multithread), "d3d11-multithread-interface");
  multithread->SetMultithreadProtected(TRUE);
  if (!multithread->GetMultithreadProtected()) {
    Fail("d3d11-multithread-protection",
         "D3D11 device did not enable multithread protection");
  }

  UINT reset_token = 0;
  Check(MFCreateDXGIDeviceManager(&reset_token, &result.manager),
        "dxgi-device-manager-create");
  Check(result.manager->ResetDevice(result.device.Get(), reset_token),
        "dxgi-device-manager-reset");
  return result;
}

std::wstring GetOptionalString(IMFAttributes* attributes, const GUID& key) {
  wchar_t* raw = nullptr;
  UINT32 length = 0;
  HRESULT result = attributes->GetAllocatedString(key, &raw, &length);
  if (result == MF_E_ATTRIBUTENOTFOUND) {
    return L"";
  }
  Check(result, "mft-activation-string");
  std::wstring value(raw, length);
  CoTaskMemFree(raw);
  return value;
}

ActivationList EnumerateHardwareEncoders(const Adapter& adapter,
                                         bool require_candidate,
                                         const GUID& format) {
  ComPtr<IMFAttributes> attributes;
  Check(MFCreateAttributes(&attributes, 1), "mft-enumeration-attributes");
  Check(attributes->SetBlob(MFT_ENUM_ADAPTER_LUID,
                            reinterpret_cast<const UINT8*>(
                                &adapter.description.AdapterLuid),
                            sizeof(LUID)),
        "mft-enumeration-adapter-luid");

  MFT_REGISTER_TYPE_INFO input = {MFMediaType_Video, MFVideoFormat_NV12};
  MFT_REGISTER_TYPE_INFO output = {MFMediaType_Video, format};
  ActivationList list;
  Check(MFTEnum2(MFT_CATEGORY_VIDEO_ENCODER,
                 MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER,
                 &input, &output, attributes.Get(), &list.values, &list.count),
        "mft-hardware-enumeration");
  if (require_candidate && list.count == 0) {
    Fail("mft-hardware-enumeration",
         "selected adapter exposed no matching hardware MFT");
  }
  return list;
}

SelectedTransform ActivateTransform(const ActivationList& list, UINT index,
                                    IMFDXGIDeviceManager* manager) {
  if (index >= list.count) {
    Fail("mft-select", "selected hardware MFT index is unavailable");
  }

  IMFActivate* activation = list.values[index];
  std::wstring friendly = GetOptionalString(activation, MFT_FRIENDLY_NAME_Attribute);
  if (friendly.empty()) {
    Fail("mft-identity", "hardware MFT has no friendly-name attribute");
  }

  GUID clsid = {};
  Check(activation->GetGUID(MFT_TRANSFORM_CLSID_Attribute, &clsid),
        "mft-clsid");

  PROPVARIANT hardware_url;
  PropVariantInit(&hardware_url);
  HRESULT hardware_result = activation->GetItem(
      MFT_ENUM_HARDWARE_URL_Attribute, &hardware_url);
  PropVariantClear(&hardware_url);
  if (FAILED(hardware_result)) {
    Fail("mft-hardware-identity",
         "hardware-only activation omitted its hardware URL attribute");
  }

  SelectedTransform selected;
  selected.activation = activation;
  selected.name = NarrowAscii(friendly);
  selected.clsid = GuidString(clsid);
  Check(activation->ActivateObject(IID_PPV_ARGS(&selected.transform)),
        "mft-activate");

  ComPtr<IMFAttributes> transform_attributes;
  Check(selected.transform->GetAttributes(&transform_attributes),
        "mft-global-attributes");
  UINT32 is_async = 0;
  Check(transform_attributes->GetUINT32(MF_TRANSFORM_ASYNC, &is_async),
        "mft-async-readback");
  if (is_async != TRUE) {
    Fail("mft-async-readback", "hardware MFT is not asynchronous");
  }
  Check(transform_attributes->SetUINT32(MF_TRANSFORM_ASYNC_UNLOCK, TRUE),
        "mft-async-unlock");

  UINT32 d3d11_aware = 0;
  Check(transform_attributes->GetUINT32(MF_SA_D3D11_AWARE, &d3d11_aware),
        "mft-d3d11-aware-readback");
  if (d3d11_aware != TRUE) {
    Fail("mft-d3d11-aware-readback", "hardware MFT is not D3D11-aware");
  }

  Check(selected.transform->ProcessMessage(
            MFT_MESSAGE_SET_D3D_MANAGER,
            reinterpret_cast<ULONG_PTR>(manager)),
        "mft-set-d3d-manager");
  Check(selected.transform.As(&selected.codec), "mft-codec-api");
  Check(selected.transform.As(&selected.events), "mft-event-generator");
  return selected;
}

void RequireProperty(ICodecAPI* codec, const GUID& key,
                     const std::string& name) {
  HRESULT supported = codec->IsSupported(&key);
  if (supported != S_OK) {
    if (FAILED(supported) && supported != E_NOTIMPL) {
      Check(supported, name + "-supported-query");
    }
    Fail(name + "-unsupported", "required codec property is unsupported");
  }
  HRESULT modifiable = codec->IsModifiable(&key);
  if (modifiable == S_FALSE) {
    Fail(name + "-readonly", "required codec property is read-only");
  }
  // Some hardware MFTs do not implement this advisory query. SetValue=S_OK
  // followed by exact GetValue readback remains the authoritative gate.
  if (modifiable != S_OK && modifiable != E_NOTIMPL) {
    Check(modifiable, name + "-modifiable-query");
  }
}

UINT32 ReadU32(ICodecAPI* codec, const GUID& key, const std::string& name) {
  VARIANT value;
  VariantInit(&value);
  Check(codec->GetValue(&key, &value), name + "-readback");
  if (value.vt != VT_UI4) {
    VariantClear(&value);
    Fail(name + "-readback-type", "codec property did not return VT_UI4");
  }
  UINT32 result = value.ulVal;
  VariantClear(&value);
  return result;
}

bool ReadBool(ICodecAPI* codec, const GUID& key, const std::string& name) {
  VARIANT value;
  VariantInit(&value);
  Check(codec->GetValue(&key, &value), name + "-readback");
  if (value.vt != VT_BOOL) {
    VariantClear(&value);
    Fail(name + "-readback-type", "codec property did not return VT_BOOL");
  }
  bool result = value.boolVal != VARIANT_FALSE;
  VariantClear(&value);
  return result;
}

void SetU32(ICodecAPI* codec, const GUID& key, UINT32 expected,
            const std::string& name) {
  RequireProperty(codec, key, name);
  VARIANT value;
  VariantInit(&value);
  value.vt = VT_UI4;
  value.ulVal = expected;
  HRESULT set_result = codec->SetValue(&key, &value);
  VariantClear(&value);
  if (set_result != S_OK) {
    if (FAILED(set_result)) {
      Check(set_result, name + "-set");
    }
    Fail(name + "-readonly", "codec property rejected the requested value");
  }
  if (ReadU32(codec, key, name) != expected) {
    Fail(name + "-weakened", "codec property readback changed the requested value");
  }
}

void SetBool(ICodecAPI* codec, const GUID& key, bool expected,
             const std::string& name) {
  RequireProperty(codec, key, name);
  VARIANT value;
  VariantInit(&value);
  value.vt = VT_BOOL;
  value.boolVal = expected ? VARIANT_TRUE : VARIANT_FALSE;
  HRESULT set_result = codec->SetValue(&key, &value);
  VariantClear(&value);
  if (set_result != S_OK) {
    if (FAILED(set_result)) {
      Check(set_result, name + "-set");
    }
    Fail(name + "-readonly", "codec property rejected the requested value");
  }
  if (ReadBool(codec, key, name) != expected) {
    Fail(name + "-weakened", "codec property readback changed the requested value");
  }
}

void SetOptionalU32(ICodecAPI* codec, const GUID& key, UINT32 value) {
  if (codec->IsSupported(&key) != S_OK) return;
  VARIANT requested;
  VariantInit(&requested);
  requested.vt = VT_UI4;
  requested.ulVal = value;
  (void)codec->SetValue(&key, &requested);
  VariantClear(&requested);
}

// Applies a property the codec may legitimately refuse; true only when it holds.
bool TrySetU32(ICodecAPI* codec, const GUID& key, UINT32 value) {
  if (codec->IsSupported(&key) != S_OK) return false;
  VARIANT requested;
  VariantInit(&requested);
  requested.vt = VT_UI4;
  requested.ulVal = value;
  const HRESULT result = codec->SetValue(&key, &requested);
  VariantClear(&requested);
  if (result != S_OK) return false;
  VARIANT actual;
  VariantInit(&actual);
  const bool holds = SUCCEEDED(codec->GetValue(&key, &actual)) && actual.vt == VT_UI4 && actual.ulVal == value;
  VariantClear(&actual);
  return holds;
}

UINT32 MaxQuantizer(OutputKind kind, const VideoProfile& profile) {
  // Bits per pixel per frame at which QP 30 stops overshooting on hard content.
  // H.264 was measured; HEVC spends about half the bits at the same quantizer.
  // AV1's scale differs and is unmeasured, so it keeps the previous behaviour.
  double threshold = 0.0;
  if (kind == OutputKind::h264) threshold = 0.075;
  else if (kind == OutputKind::h265) threshold = 0.04;
  const double pixel_rate = static_cast<double>(profile.width) * profile.height * profile.frame_rate;
  if (threshold == 0.0 || pixel_rate <= 0.0) return VideoProfile::kMaxQuantizer;
  return profile.bit_rate / pixel_rate >= threshold ? VideoProfile::kMaxQuantizer
                                                    : VideoProfile::kQuantizerUnbounded;
}

UINT32 TemporalLayerCount(OutputKind kind, const VideoProfile& profile) {
  const bool layered = kind == OutputKind::h265 || kind == OutputKind::h264;
  return layered && profile.frame_rate >= 100 ? 2 : 1;
}

void ConfigureCodec(
    ICodecAPI* codec,
    const VideoProfile& profile,
    OutputKind kind) {
  // Variable mode asks for peak-constrained VBR and falls back to CBR on an
  // encoder that refuses it; Constant mode is always CBR.
  if (profile.constant_bitrate ||
      !TrySetU32(codec, CODECAPI_AVEncCommonRateControlMode,
                 eAVEncCommonRateControlMode_PeakConstrainedVBR)) {
    SetU32(codec, CODECAPI_AVEncCommonRateControlMode,
           eAVEncCommonRateControlMode_CBR, "codec-cbr");
  }
  SetBool(codec, CODECAPI_AVLowLatencyMode, true, "codec-low-latency");
  SetU32(codec, CODECAPI_AVEncCommonMeanBitRate, profile.mean_bit_rate(),
         "codec-mean-bitrate");
  (void)TrySetU32(codec, CODECAPI_AVEncCommonMaxBitRate, profile.bit_rate);
  // A tight buffer keeps constant mode from borrowing bits across frames.
  if (profile.constant_bitrate && profile.frame_rate > 0) {
    (void)TrySetU32(codec, CODECAPI_AVEncCommonBufferSize,
                    std::max<UINT32>(1, profile.bit_rate / profile.frame_rate * 2));
  }
  // No periodic IDR: a recovery key frame is requested only when a viewer
  // needs one. Each periodic IDR was a burst that broke a flat bitrate.
  (void)TrySetU32(codec, CODECAPI_AVEncMPVGOPSize, 0xFFFFFFFFu);
  SetOptionalU32(codec, CODECAPI_AVEncCommonQualityVsSpeed,
                 profile.quality_vs_speed());
  // Quality floor: a hard frame may spend more of the budget instead of turning
  // to mush. Measured on a 120 fps clip at 38 Mbps: average QP 26 -> 20 with the
  // bitrate unchanged and every frame still delivered. Off under a tight budget.
  (void)TrySetU32(codec, CODECAPI_AVEncVideoMaxQP, MaxQuantizer(kind, profile));
  RequireProperty(codec, CODECAPI_AVEncVideoForceKeyFrame,
                  "codec-force-keyframe");
  // Optional: an encoder without temporal layers still serves every viewer at
  // the full rate; only the per-viewer cap then has nothing to drop.
  if (const UINT32 layers = TemporalLayerCount(kind, profile); layers > 1) {
    (void)TrySetU32(codec, CODECAPI_AVEncVideoTemporalLayerCount, layers);
  }
}

ComPtr<IMFMediaType> CreateOutputType(
    const VideoProfile& profile, OutputKind kind) {
  ComPtr<IMFMediaType> type;
  Check(MFCreateMediaType(&type), "output-type-create");
  Check(type->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video),
        "output-type-major");
  Check(type->SetGUID(MF_MT_SUBTYPE, EncoderSubtype(kind)),
        "output-type-subtype");
  if (IsNewHardwareCodec(kind)) SetAv1VideoColor(type.Get());
  else SetSdrVideoColor(type.Get());
  Check(type->SetUINT32(MF_MT_AVG_BITRATE, profile.bit_rate),
        "output-type-bitrate");
  Check(MFSetAttributeSize(type.Get(), MF_MT_FRAME_SIZE, profile.width,
                           profile.height),
        "output-type-size");
  Check(MFSetAttributeRatio(type.Get(), MF_MT_FRAME_RATE, profile.frame_rate, 1),
        "output-type-framerate");
  Check(MFSetAttributeRatio(type.Get(), MF_MT_PIXEL_ASPECT_RATIO, 1, 1),
        "output-type-pixel-aspect");
  Check(type->SetUINT32(MF_MT_INTERLACE_MODE,
                        MFVideoInterlace_Progressive),
        "output-type-interlace");
  if (kind == OutputKind::av1) {
    // The encoder selects the AV1 level for the configured size and rate.
    Check(type->SetUINT32(MF_MT_VIDEO_PROFILE, eAVEncAV1VProfile_Main_420_8),
          "output-type-profile");
    return type;
  }
  if (kind == OutputKind::h265) {
    // Main 8-bit 4:2:0; the encoder selects the level for size, rate and bitrate.
    Check(type->SetUINT32(MF_MT_VIDEO_PROFILE, eAVEncH265VProfile_Main_420_8),
          "output-type-profile");
    return type;
  }
  Check(type->SetUINT32(MF_MT_MPEG2_PROFILE, eAVEncH264VProfile_Base),
        "output-type-profile");
  Check(type->SetUINT32(MF_MT_MPEG2_LEVEL, profile.h264_level()),
        "output-type-level");
  return type;
}

ComPtr<IMFMediaType> CreateInputType(
    const VideoProfile& profile, OutputKind kind) {
  ComPtr<IMFMediaType> type;
  Check(MFCreateMediaType(&type), "input-type-create");
  Check(type->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video),
        "input-type-major");
  Check(type->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_NV12),
        "input-type-subtype");
  if (IsNewHardwareCodec(kind)) SetAv1VideoColor(type.Get());
  else SetSdrVideoColor(type.Get());
  Check(MFSetAttributeSize(type.Get(), MF_MT_FRAME_SIZE, profile.width,
                           profile.height),
        "input-type-size");
  Check(MFSetAttributeRatio(type.Get(), MF_MT_FRAME_RATE, profile.frame_rate, 1),
        "input-type-framerate");
  Check(MFSetAttributeRatio(type.Get(), MF_MT_PIXEL_ASPECT_RATIO, 1, 1),
        "input-type-pixel-aspect");
  Check(type->SetUINT32(MF_MT_INTERLACE_MODE,
                        MFVideoInterlace_Progressive),
        "input-type-interlace");
  Check(type->SetUINT32(MF_MT_DEFAULT_STRIDE, profile.width),
        "input-type-stride");
  return type;
}

void RequireTypeU32(IMFAttributes* type, const GUID& key, UINT32 expected,
                    const std::string& stage) {
  UINT32 actual = 0;
  Check(type->GetUINT32(key, &actual), stage + "-readback");
  if (actual != expected) {
    Fail(stage + "-weakened", "media type changed a required value");
  }
}

void ValidateMediaTypes(
    IMFTransform* transform,
    const VideoProfile& profile,
    OutputKind kind) {
  ComPtr<IMFMediaType> output;
  Check(transform->GetOutputCurrentType(0, &output),
        "output-type-current");
  GUID subtype = {};
  Check(output->GetGUID(MF_MT_SUBTYPE, &subtype), "output-type-subtype-readback");
  if (subtype != EncoderSubtype(kind)) {
    Fail("output-type-subtype-weakened", "output is not the selected codec");
  }
  RequireTypeU32(output.Get(), MF_MT_AVG_BITRATE, profile.bit_rate,
                 "output-type-bitrate");
  if (kind == OutputKind::av1) {
    RequireTypeU32(output.Get(), MF_MT_VIDEO_PROFILE,
                   eAVEncAV1VProfile_Main_420_8, "output-type-profile");
  } else if (kind == OutputKind::h265) {
    RequireTypeU32(output.Get(), MF_MT_VIDEO_PROFILE,
                   eAVEncH265VProfile_Main_420_8, "output-type-profile");
  } else {
    RequireTypeU32(output.Get(), MF_MT_MPEG2_PROFILE,
                   eAVEncH264VProfile_Base, "output-type-profile");
    RequireTypeU32(output.Get(), MF_MT_MPEG2_LEVEL, profile.h264_level(),
                   "output-type-level");
  }

  UINT32 width = 0;
  UINT32 height = 0;
  Check(MFGetAttributeSize(output.Get(), MF_MT_FRAME_SIZE, &width, &height),
        "output-type-size-readback");
  if (width != profile.width || height != profile.height) {
    Fail("output-type-size-weakened", "output dimensions changed");
  }
  UINT32 numerator = 0;
  UINT32 denominator = 0;
  Check(MFGetAttributeRatio(output.Get(), MF_MT_FRAME_RATE, &numerator,
                            &denominator),
        "output-type-framerate-readback");
  if (numerator != profile.frame_rate || denominator != 1) {
    Fail("output-type-framerate-weakened", "output frame rate changed");
  }
}

void ValidateCodecReadback(
    ICodecAPI* codec,
    const VideoProfile& profile) {
  const UINT32 mode = ReadU32(codec, CODECAPI_AVEncCommonRateControlMode, "codec-cbr");
  if ((mode != eAVEncCommonRateControlMode_CBR &&
       (profile.constant_bitrate || mode != eAVEncCommonRateControlMode_PeakConstrainedVBR)) ||
      !ReadBool(codec, CODECAPI_AVLowLatencyMode, "codec-low-latency") ||
      ReadU32(codec, CODECAPI_AVEncCommonMeanBitRate,
              "codec-mean-bitrate") != profile.mean_bit_rate()) {
    Fail("codec-final-readback", "one or more codec properties were weakened");
  }
}

std::optional<std::pair<size_t, size_t>> FindStartCode(
    const std::vector<UINT8>& bytes, size_t from) {
  for (size_t index = from; index + 3 <= bytes.size(); ++index) {
    if (bytes[index] != 0 || bytes[index + 1] != 0) {
      continue;
    }
    if (bytes[index + 2] == 1) {
      return std::pair<size_t, size_t>{index, 3};
    }
    if (index + 4 <= bytes.size() && bytes[index + 2] == 0 &&
        bytes[index + 3] == 1) {
      return std::pair<size_t, size_t>{index, 4};
    }
  }
  return std::nullopt;
}

NalSummary InspectAnnexB(const std::vector<UINT8>& bytes) {
  NalSummary summary;
  std::optional<std::pair<size_t, size_t>> current = FindStartCode(bytes, 0);
  if (!current || current->first != 0) {
    return summary;
  }
  summary.annex_b = true;
  bool slices = false, referenced = false;
  while (current) {
    size_t nal_start = current->first + current->second;
    std::optional<std::pair<size_t, size_t>> next = FindStartCode(bytes, nal_start);
    size_t nal_end = next ? next->first : bytes.size();
    if (nal_start < nal_end) {
      UINT8 nal_type = bytes[nal_start] & 0x1f;
      summary.sps = summary.sps || nal_type == 7;
      summary.pps = summary.pps || nal_type == 8;
      summary.idr = summary.idr || nal_type == 5;
      if (nal_type == 1 || nal_type == 5) {
        slices = true;
        referenced = referenced || nal_type == 5 || (bytes[nal_start] & 0x60) != 0;
      }
      // SVC prefix NAL: temporal_id is the top three bits of its fourth byte.
      if (nal_type == 14 && nal_end - nal_start >= 4) {
        summary.temporal_id = std::max<UINT8>(summary.temporal_id, bytes[nal_start + 3] >> 5);
      }
      if (nal_type == 7 && nal_end - nal_start >= 4) {
        std::string value = ProfileLevelId(bytes[nal_start + 1],
                                           bytes[nal_start + 2],
                                           bytes[nal_start + 3]);
        if (summary.profile_level_id && *summary.profile_level_id != value) {
          Fail("bitstream-sps-consistency",
               "one access unit contained conflicting SPS profile bytes");
        }
        summary.profile_level_id = value;
      }
    }
    current = next;
  }
  summary.non_reference = slices && !referenced;
  return summary;
}

// "type/tid" per NAL unit, for diagnostics only.
std::string DescribeHevcNals(const std::vector<UINT8>& bytes) {
  std::string out;
  for (auto current = FindStartCode(bytes, 0); current;) {
    const size_t start = current->first + current->second;
    const auto next = FindStartCode(bytes, start);
    if (start + 1 < bytes.size()) {
      if (!out.empty()) out += ' ';
      out += std::to_string((bytes[start] >> 1) & 0x3f) + "/" + std::to_string((bytes[start + 1] & 0x07) - 1);
    }
    current = next;
  }
  return out;
}

HevcSummary InspectHevc(const std::vector<UINT8>& bytes) {
  HevcSummary summary;
  std::optional<std::pair<size_t, size_t>> current = FindStartCode(bytes, 0);
  if (!current || current->first != 0) return summary;
  summary.annex_b = true;
  bool vps = false, sps = false, pps = false;
  while (current) {
    const size_t nal_start = current->first + current->second;
    const auto next = FindStartCode(bytes, nal_start);
    const size_t nal_end = next ? next->first : bytes.size();
    // A NAL unit header is two bytes with forbidden_zero_bit clear.
    if (nal_end - nal_start < 2 || (bytes[nal_start] & 0x80) != 0) {
      summary.annex_b = false;
      return summary;
    }
    const UINT8 type = (bytes[nal_start] >> 1) & 0x3f;
    vps = vps || type == 32;
    sps = sps || type == 33;
    pps = pps || type == 34;
    summary.irap = summary.irap || (type >= 16 && type <= 21);
    summary.picture = summary.picture || type <= 21;
    // nuh_temporal_id_plus1 is 1..7; zero would be a temporal id of -1.
    if ((bytes[nal_start + 1] & 0x07) == 0) {
      summary.annex_b = false;
      return summary;
    }
    if (type <= 31) {
      summary.temporal_id = std::max<UINT8>(summary.temporal_id, (bytes[nal_start + 1] & 0x07) - 1);
    }
    current = next;
  }
  summary.parameter_sets = vps && sps && pps;
  return summary;
}

// Walks size-delimited OBUs (AV1 spec 5.3). Every OBU must carry obu_size and
// the walk must end exactly at the access unit's end.
ObuSummary InspectObus(const std::vector<UINT8>& bytes) {
  ObuSummary summary;
  size_t at = 0;
  while (at < bytes.size()) {
    const UINT8 header = bytes[at];
    if ((header & 0x80) != 0 || (header & 0x02) == 0) return summary;
    const UINT8 type = (header >> 3) & 0x0f;
    size_t cursor = at + 1 + ((header & 0x04) != 0 ? 1 : 0);
    UINT64 size = 0;
    bool terminated = false;
    for (int index = 0; index < 8 && cursor < bytes.size(); ++index) {
      const UINT8 value = bytes[cursor++];
      size |= static_cast<UINT64>(value & 0x7f) << (7 * index);
      if ((value & 0x80) == 0) {
        terminated = true;
        break;
      }
    }
    if (!terminated || size > bytes.size() - cursor) return summary;
    summary.sequence_header = summary.sequence_header || type == 1;
    summary.frame = summary.frame || type == 3 || type == 6;
    at = cursor + static_cast<size_t>(size);
  }
  summary.valid = !bytes.empty();
  return summary;
}

std::vector<UINT8> ReadSample(IMFSample* sample) {
  DWORD total_length = 0;
  Check(sample->GetTotalLength(&total_length), "output-total-length");
  if (total_length == 0 || total_length > kMaxEncodedSampleBytes) {
    Fail("output-buffer-bounds", "encoded output size is empty or unbounded");
  }

  ComPtr<IMFMediaBuffer> buffer;
  Check(sample->ConvertToContiguousBuffer(&buffer), "output-contiguous-buffer");
  DWORD length = 0;
  Check(buffer->GetCurrentLength(&length), "output-buffer-length");
  if (length != total_length) {
    Fail("output-buffer-length", "contiguous output length changed unexpectedly");
  }
  std::vector<UINT8> bytes(length);
  UINT8* raw = nullptr;
  DWORD maximum = 0;
  DWORD current = 0;
  Check(buffer->Lock(&raw, &maximum, &current), "output-buffer-lock");
  if (raw == nullptr || current != length || current > kMaxEncodedSampleBytes) {
    buffer->Unlock();
    Fail("output-buffer-lock-bounds",
         "locked output length is inconsistent or unbounded");
  }
  std::copy(raw, raw + current, bytes.begin());
  Check(buffer->Unlock(), "output-buffer-unlock");
  return bytes;
}

void ForceKeyFrame(ICodecAPI* codec) {
  VARIANT value;
  VariantInit(&value);
  value.vt = VT_UI4;
  value.ulVal = 1;
  HRESULT set_result =
      codec->SetValue(&CODECAPI_AVEncVideoForceKeyFrame, &value);
  VariantClear(&value);
  if (set_result != S_OK) {
    if (FAILED(set_result)) {
      Check(set_result, "codec-force-keyframe-set");
    }
    Fail("codec-force-keyframe-readonly",
         "force-key-frame command was not accepted");
  }
}

ComPtr<IMFSample> CreateCallerOutputSample(const MFT_OUTPUT_STREAM_INFO& info) {
  if (info.cbSize > kMaxEncodedSampleBytes) {
    Fail("output-buffer-contract",
         "hardware MFT requested an output buffer larger than four MiB");
  }
  ComPtr<IMFSample> sample;
  Check(MFCreateSample(&sample), "output-sample-create");
  ComPtr<IMFMediaBuffer> buffer;
  // Stream info uses bytes; the allocator takes an alignment mask (bytes - 1).
  Check(MFCreateAlignedMemoryBuffer(kMaxEncodedSampleBytes,
                                    info.cbAlignment ? info.cbAlignment - 1 : 0,
                                    &buffer),
        "output-memory-buffer");
  Check(sample->AddBuffer(buffer.Get()), "output-sample-buffer");
  return sample;
}

ComPtr<IMFSample> PullOutput(IMFTransform* transform,
                             const MFT_OUTPUT_STREAM_INFO& info) {
  bool transform_provides =
      (info.dwFlags & (MFT_OUTPUT_STREAM_PROVIDES_SAMPLES |
                       MFT_OUTPUT_STREAM_CAN_PROVIDE_SAMPLES)) != 0;
  ComPtr<IMFSample> caller_sample;
  if (!transform_provides) {
    caller_sample = CreateCallerOutputSample(info);
  }

  MFT_OUTPUT_DATA_BUFFER output = {};
  output.dwStreamID = 0;
  output.pSample = caller_sample.Get();
  DWORD status = 0;
  HRESULT result = transform->ProcessOutput(0, 1, &output, &status);

  ComPtr<IMFCollection> events;
  if (output.pEvents != nullptr) {
    events.Attach(output.pEvents);
  }
  Check(result, "mft-process-output");
  if ((output.dwStatus & MFT_OUTPUT_DATA_BUFFER_FORMAT_CHANGE) != 0 ||
      (status & MFT_PROCESS_OUTPUT_STATUS_NEW_STREAMS) != 0) {
    Fail("mft-output-format-change",
         "hardware MFT changed format during the fixed run");
  }

  if (caller_sample) {
    if (output.pSample != caller_sample.Get()) {
      if (output.pSample != nullptr) {
        output.pSample->Release();
      }
      Fail("mft-output-allocation",
           "hardware MFT replaced the caller-owned output sample");
    }
    return caller_sample;
  }
  if (output.pSample == nullptr) {
    Fail("mft-output-sample", "hardware MFT produced no output sample");
  }
  ComPtr<IMFSample> produced;
  produced.Attach(output.pSample);
  return produced;
}

ComPtr<IMFSample> CreateSurfaceSample(ID3D11Texture2D* texture,
                                      LONGLONG timestamp100ns,
                                      LONGLONG duration100ns) {
  ComPtr<IMFMediaBuffer> buffer;
  Check(MFCreateDXGISurfaceBuffer(IID_ID3D11Texture2D, texture, 0, FALSE,
                                  &buffer),
        "input-dxgi-buffer");
  // Surface storage exists, but the wrapper initially reports zero valid bytes.
  // MFTs may honor that length instead of inferring it from the texture.
  DWORD length = 0;
  Check(buffer->GetMaxLength(&length), "input-buffer-capacity");
  Check(buffer->SetCurrentLength(length), "input-buffer-length");
  ComPtr<IMFSample> sample;
  Check(MFCreateVideoSampleFromSurface(nullptr, &sample),
        "input-video-sample");
  Check(sample->AddBuffer(buffer.Get()), "input-sample-buffer");
  Check(sample->SetSampleTime(timestamp100ns), "input-sample-time");
  Check(sample->SetSampleDuration(duration100ns), "input-sample-duration");
  return sample;
}

void RequireEncoderTime(EncoderClock::time_point deadline) {
  if (EncoderClock::now() >= deadline) {
    Fail("codec-probe-timeout", "native codec selection exceeded its time budget");
  }
}

LiveEncoder::LiveEncoder(
    SelectedTransform selected,
    VideoProfile profile,
    OutputKind codec)
    : VideoEncoder(codec, selected.name, selected.clsid),
      selected_(std::move(selected)), events_(selected_.events.Get()), profile_(profile), configured_(profile) {
  if (kind != OutputKind::h264 && !IsNewHardwareCodec(kind))
    Fail("mft-codec", "hardware encoder codec is unsupported");
  ConfigureCodec(selected_.codec.Get(), profile_, kind);
  max_quantizer_ = MaxQuantizer(kind, profile_);
  ComPtr<IMFMediaType> output_type = CreateOutputType(profile_, kind);
  Check(selected_.transform->SetOutputType(0, output_type.Get(), 0),
        "mft-set-output-type");
  ComPtr<IMFMediaType> input_type = CreateInputType(profile_, kind);
  Check(selected_.transform->SetInputType(0, input_type.Get(), 0),
        "mft-set-input-type");
  ValidateMediaTypes(selected_.transform.Get(), profile_, kind);
  ValidateCodecReadback(selected_.codec.Get(), profile_);
  Check(selected_.transform->GetOutputStreamInfo(0, &output_info_),
        "mft-output-stream-info");
  Check(selected_.transform->ProcessMessage(
            MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0),
        "mft-begin-streaming");
  Check(selected_.transform->ProcessMessage(
            MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0),
        "mft-start-stream");
  started_ = true;
}

LiveEncoder::~LiveEncoder() {
  if (started_) {
    selected_.transform->ProcessMessage(MFT_MESSAGE_COMMAND_FLUSH, 0);
    selected_.transform->ProcessMessage(MFT_MESSAGE_NOTIFY_END_STREAMING, 0);
  }
}

void LiveEncoder::SetBitrate(UINT32 bitrate) {
  profile_.bit_rate = bitrate;
  SetU32(selected_.codec.Get(), CODECAPI_AVEncCommonMeanBitRate, profile_.mean_bit_rate(),
         "codec-live-bitrate");
  (void)TrySetU32(selected_.codec.Get(), CODECAPI_AVEncCommonMaxBitRate, bitrate);
  // The floor depends on the budget: a live cut must not leave it pinning the
  // quantizer under a ceiling it can no longer meet.
  const UINT32 floor = MaxQuantizer(kind, profile_);
  if (floor != max_quantizer_ &&
      TrySetU32(selected_.codec.Get(), CODECAPI_AVEncVideoMaxQP, floor)) {
    max_quantizer_ = floor;
  }
}

EncodedAccessUnit LiveEncoder::Encode(
    ID3D11Texture2D* texture, UINT64 timestamp100ns, bool force_key_frame,
    EncoderClock::time_point probe_deadline) {
  WaitForInput(probe_deadline);
  if (force_key_frame) ForceKeyFrame(selected_.codec.Get());
  ComPtr<IMFSample> sample = CreateSurfaceSample(
      texture, static_cast<LONGLONG>(timestamp100ns),
      profile_.frame_duration_100ns());
  Check(selected_.transform->ProcessInput(0, sample.Get(), 0),
        "mft-process-input");
  --input_requests_;

  const auto deadline = std::min(probe_deadline,
      EncoderClock::now() + std::chrono::seconds(2));
  while (std::chrono::steady_clock::now() < deadline) {
    MediaEventType type = NextEvent(deadline, "mft-output-timeout");
    if (type == METransformNeedInput) {
      ++input_requests_;
      continue;
    }
    if (type != METransformHaveOutput) continue;

    ComPtr<IMFSample> output = PullOutput(selected_.transform.Get(),
                                          output_info_);
    LONGLONG output_time = -1;
    Check(output->GetSampleTime(&output_time), "output-sample-time");
    if (output_time != static_cast<LONGLONG>(timestamp100ns)) {
      Fail("output-order", "hardware MFT changed or reordered a live timestamp");
    }
    std::vector<UINT8> bytes = ReadSample(output.Get());
    if (kind == OutputKind::av1) {
      const ObuSummary obus = InspectObus(bytes);
      if (!obus.valid || !obus.frame) {
        Fail("bitstream-obu", "live output is not size-delimited AV1 OBUs");
      }
      // A recovery point repeats the sequence header with an intra frame.
      const bool key_frame = MFGetAttributeUINT32(output.Get(), MFSampleExtension_CleanPoint, 0) != 0 &&
          obus.sequence_header;
      if (force_key_frame && !key_frame) {
        Fail("bitstream-recovery-unit", "requested live recovery unit lacks an AV1 key frame");
      }
      sequence_header_seen_ = sequence_header_seen_ || obus.sequence_header;
      if (!sequence_header_seen_) {
        Fail("bitstream-sequence-header", "first live AV1 access unit lacked a sequence header");
      }
      RequireEncoderTime(probe_deadline);
      return EncodedAccessUnit{timestamp100ns, key_frame, std::move(bytes)};
    }
    if (kind == OutputKind::h265) {
      const HevcSummary hevc = InspectHevc(bytes);
      if (!hevc.annex_b || !hevc.picture) {
        Fail("bitstream-annexb", "live output is not Annex-B HEVC");
      }
      // A recovery point carries VPS/SPS/PPS with an IRAP picture.
      const bool key_frame = hevc.irap && hevc.parameter_sets;
      // With temporal layers an enhancement slot cannot become a key frame; the
      // encoder makes the next base-layer frame one, so the caller asks again.
      if (force_key_frame && !key_frame && hevc.temporal_id == 0) {
        Fail("bitstream-recovery-unit", "requested live recovery unit lacks VPS, SPS, PPS, or IRAP (NAL " +
                                            DescribeHevcNals(bytes) + ")");
      }
      sequence_header_seen_ = sequence_header_seen_ || hevc.parameter_sets;
      if (!sequence_header_seen_) {
        Fail("bitstream-sps", "first live HEVC access unit did not carry VPS, SPS and PPS");
      }
      RequireEncoderTime(probe_deadline);
      return EncodedAccessUnit{timestamp100ns, key_frame, std::move(bytes)};
    }
    NalSummary nal = InspectAnnexB(bytes);
    if (!nal.annex_b) {
      Fail("bitstream-annexb", "live output is not Annex-B H264");
    }
    if (nal.profile_level_id) {
      if (!configured_.accepts_h264_profile_level_id(*nal.profile_level_id)) {
        Fail("bitstream-profile", "hardware MFT changed the requested H.264 profile level " +
                                      configured_.profile_level_id() + " to " + *nal.profile_level_id);
      }
      if (profile_level_id_ && *profile_level_id_ != *nal.profile_level_id) {
        Fail("bitstream-profile-change", "hardware MFT changed SPS profile");
      }
      profile_level_id_ = nal.profile_level_id;
    }
    // An enhancement-layer slot cannot become a key frame; the request carries
    // to the next base-layer frame (adaptive_encoder keeps it pending).
    if (force_key_frame && !(nal.sps && nal.pps && nal.idr) && nal.temporal_id == 0 &&
        !nal.non_reference) {
      Fail("bitstream-recovery-unit",
           "requested live recovery unit lacks SPS, PPS, or IDR");
    }
    if (!profile_level_id_) {
      Fail("bitstream-sps", "first live access unit did not carry an SPS");
    }
    RequireEncoderTime(probe_deadline);
    return EncodedAccessUnit{timestamp100ns, nal.idr && nal.sps && nal.pps,
                             std::move(bytes)};
  }
  Fail("mft-output-timeout", "hardware MFT did not produce live output in time");
}

MediaEventType LiveEncoder::NextEvent(
    EncoderClock::time_point deadline, const char* timeout_stage) {
  ComPtr<IMFMediaEvent> event = events_.Read(deadline, timeout_stage);
  HRESULT status = S_OK;
  Check(event->GetStatus(&status), "mft-event-status-read");
  Check(status, "mft-event-status");
  MediaEventType type = MEUnknown;
  Check(event->GetType(&type), "mft-event-type");
  if (type == MEError) Fail("mft-error-event", "hardware MFT emitted MEError");
  return type;
}

void LiveEncoder::WaitForInput(EncoderClock::time_point probe_deadline) {
  const auto deadline = std::min(probe_deadline,
      EncoderClock::now() + std::chrono::seconds(2));
  while (input_requests_ == 0) {
    MediaEventType type = NextEvent(deadline, "mft-input-timeout");
    if (type == METransformNeedInput) {
      ++input_requests_;
    } else if (type == METransformHaveOutput) {
      Fail("mft-unexpected-output", "hardware MFT produced output without live input");
    }
  }
}

}  // namespace piik::capture::windows
