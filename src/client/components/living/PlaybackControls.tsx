import { useEffect, useRef, useState, type RefObject, type ReactNode } from "react";
import { ViewerAudio, type ViewerAudioSnapshot } from "../../media/viewer-audio";
import {
  LosslessAudioPlayer,
  losslessPlaybackSupported,
  type LosslessStopReason,
} from "../../media/lossless-audio";
import { useCopy } from "../../ui/copy";
import { debugError } from "../../lib/debug";
import { Btn } from "./primitives";
import { Tooltip } from "./Tooltip";
import { usePlaybackControlsVisibility } from "./use-playback-controls-visibility";
import { usePictureInPicture } from "./use-picture-in-picture";
import { bindPlaybackGestures } from "./playback-gestures";
import { toggleVideoFullscreen, videoFullscreenState, type FullscreenVideo } from "./video-fullscreen";
import "./playback-controls.css";

// Reflect the existing media element; binding, autoplay recovery and room
// presentation retain their existing owners.
export function PlaybackControls({
  videoRef, stream, audioTrackKey, canPlay, theaterMode, onPlay,
  onToggleTheater, onReconnect, reconnectAvailable,
  extraActions, losslessChannel = null, playoutDelayMs = null,
}: {
  videoRef: RefObject<HTMLVideoElement | null>;
  stream: MediaStream | null;
  audioTrackKey?: string;
  canPlay: boolean;
  theaterMode: boolean;
  onPlay: () => void;
  onToggleTheater: () => void;
  onReconnect: () => void;
  reconnectAvailable: boolean;
  extraActions?: ReactNode;
  /** An App Host's optional lossless channel on the current connection. */
  losslessChannel?: RTCDataChannel | null;
  /** The received picture's playout delay, which lossless audio follows. */
  playoutDelayMs?: number | null;
}) {
  const audioRef = useRef<ViewerAudio | null>(null);
  const losslessRef = useRef<LosslessAudioPlayer | null>(null);
  const [losslessSupported, setLosslessSupported] = useState(false);
  const [channelOpen, setChannelOpen] = useState(false);
  const [lossless, setLossless] = useState<"off" | "starting" | "playing">("off");
  const [losslessIssue, setLosslessIssue] = useState<LosslessStopReason | null>(null);
  const controlsRef = useRef<HTMLDivElement | null>(null);
  const [audio, setAudio] = useState<ViewerAudioSnapshot>({
    level: 1, muted: false, boostAvailable: true,
  });
  const [paused, setPaused] = useState(true);
  const [hasAudio, setHasAudio] = useState(false);
  const [fullscreen, setFullscreen] = useState({ supported: false, ready: false, active: false });
  const [fullscreenFailed, setFullscreenFailed] = useState(false);
  const hidden = usePlaybackControlsVisibility(videoRef, controlsRef, paused || !canPlay);
  const picture = usePictureInPicture(videoRef);

  const togglePlayback = () => {
    if (!canPlay) return;
    const video = videoRef.current;
    if (!video) return;
    if (video.paused) {
      audioRef.current?.resume();
      onPlay();
    } else {
      video.pause();
    }
  };
  const toggleFullscreen = () => {
    if (theaterMode) return;
    const video = videoRef.current as FullscreenVideo | null;
    if (!video) return;
    setFullscreenFailed(false);
    void toggleVideoFullscreen(video).catch((error: unknown) => {
      debugError("playback", "fullscreen-failed", error);
      if (videoRef.current === video) setFullscreenFailed(true);
    });
  };
  const actionsRef = useRef({ togglePlayback, toggleFullscreen });
  actionsRef.current = { togglePlayback, toggleFullscreen };

  useEffect(() => {
    const video = videoRef.current as FullscreenVideo | null;
    if (!video) return;
    const output = new ViewerAudio(video, setAudio);
    audioRef.current = output;
    const syncPlayback = () => setPaused(video.paused);
    const syncFullscreen = () => {
      const state = videoFullscreenState(video);
      setFullscreen(state);
      setFullscreenFailed(false);
      if (state.nativeActive) output.useNativeControls();
    };
    const fullscreenEvents = ["loadedmetadata", "loadeddata", "emptied", "webkitbeginfullscreen", "webkitendfullscreen", "webkitpresentationmodechanged"];
    const onKey = (event: KeyboardEvent) => {
      if (event.code !== "Space" || event.target !== video) return;
      event.preventDefault();
      actionsRef.current.togglePlayback();
    };
    syncPlayback();
    syncFullscreen();
    for (const event of ["play", "pause", "ended", "emptied"]) video.addEventListener(event, syncPlayback);
    for (const event of fullscreenEvents) video.addEventListener(event, syncFullscreen);
    video.addEventListener("keydown", onKey);
    document.addEventListener("fullscreenchange", syncFullscreen);
    const unbindGestures = bindPlaybackGestures(video,
      () => actionsRef.current.togglePlayback(),
      () => actionsRef.current.toggleFullscreen());
    return () => {
      unbindGestures();
      output.dispose();
      audioRef.current = null;
      for (const event of ["play", "pause", "ended", "emptied"]) video.removeEventListener(event, syncPlayback);
      for (const event of fullscreenEvents) video.removeEventListener(event, syncFullscreen);
      video.removeEventListener("keydown", onKey);
      document.removeEventListener("fullscreenchange", syncFullscreen);
    };
  }, [videoRef]);

  useEffect(() => {
    audioRef.current?.bind(stream);
    const track = stream?.getAudioTracks()[0];
    const syncAudio = () => setHasAudio(Boolean(track && track.readyState !== "ended"));
    syncAudio();
    track?.addEventListener("ended", syncAudio);
    return () => track?.removeEventListener("ended", syncAudio);
  }, [stream, audioTrackKey]);

  useEffect(() => {
    let current = true;
    void losslessPlaybackSupported().then((supported) => {
      if (current) setLosslessSupported(supported);
    });
    return () => { current = false; };
  }, []);

  // A new connection brings a new channel; the old player ends with the old one.
  useEffect(() => {
    const channel = losslessChannel;
    const sync = () => setChannelOpen(channel?.readyState === "open");
    sync();
    channel?.addEventListener("open", sync);
    channel?.addEventListener("close", sync);
    return () => {
      channel?.removeEventListener("open", sync);
      channel?.removeEventListener("close", sync);
      losslessRef.current?.stop();
      losslessRef.current = null;
      audioRef.current?.setExternalOutput(null);
      setLossless("off");
    };
  }, [losslessChannel]);

  useEffect(() => {
    if (playoutDelayMs !== null) losslessRef.current?.setTargetDelayMs(playoutDelayMs);
  }, [playoutDelayMs, lossless]);

  const toggleLossless = () => {
    const active = losslessRef.current;
    if (active) {
      active.stop();
      losslessRef.current = null;
      audioRef.current?.setExternalOutput(null);
      setLossless("off");
      return;
    }
    if (!losslessChannel || losslessChannel.readyState !== "open") return;
    const player = new LosslessAudioPlayer(losslessChannel, {
      // Opus stays audible until the lossless buffer holds the playout delay.
      onPlaying: () => {
        if (losslessRef.current !== player) return;
        audioRef.current?.setExternalOutput(player);
        setLossless("playing");
      },
      onStopped: (reason) => {
        if (losslessRef.current !== player) return;
        losslessRef.current = null;
        audioRef.current?.setExternalOutput(null);
        setLossless("off");
        setLosslessIssue(reason);
      },
    });
    player.setVolume(0);
    losslessRef.current = player;
    setLossless("starting");
    setLosslessIssue(null);
    if (playoutDelayMs !== null) player.setTargetDelayMs(playoutDelayMs);
    void player.start();
  };

  const silent = audio.muted || audio.level === 0;
  const losslessAvailable = losslessSupported && channelOpen && hasAudio;

  return <PlaybackControlsView controlsRef={controlsRef} hidden={hidden} audio={audio}
    paused={paused} hasAudio={hasAudio} canPlay={canPlay} theaterMode={theaterMode}
    fullscreen={fullscreen} fullscreenFailed={fullscreenFailed} picture={picture}
    togglePlayback={togglePlayback} toggleFullscreen={toggleFullscreen}
    toggleMute={() => {
      if (audio.level === 0) audioRef.current?.setLevel(1);
      audioRef.current?.setMuted(!silent);
    }} setLevel={level => {
      audioRef.current?.setLevel(level);
      audioRef.current?.setMuted(false);
    }} onToggleTheater={onToggleTheater} onReconnect={onReconnect}
    reconnectAvailable={reconnectAvailable} extraActions={extraActions}
    lossless={losslessAvailable || lossless !== "off" ? { state: lossless, issue: losslessIssue, toggle: toggleLossless } : null} />;
}

// The film shares the rendered controls; the component above alone owns media effects.
export function PlaybackControlsView({
  controlsRef, hidden, audio, paused, hasAudio, canPlay, theaterMode,
  fullscreen, fullscreenFailed, picture, togglePlayback, toggleFullscreen,
  toggleMute, setLevel, onToggleTheater, onReconnect, reconnectAvailable, extraActions,
  lossless = null,
}: {
  controlsRef?: RefObject<HTMLDivElement | null>; hidden?: boolean;
  audio: ViewerAudioSnapshot; paused: boolean; hasAudio: boolean; canPlay: boolean;
  theaterMode: boolean; fullscreen: { supported: boolean; ready: boolean; active: boolean };
  fullscreenFailed?: boolean;
  picture: Pick<ReturnType<typeof usePictureInPicture>, "active" | "supported" | "failed" | "toggle">;
  togglePlayback: () => void; toggleFullscreen: () => void; toggleMute: () => void;
  setLevel: (level: number) => void; onToggleTheater: () => void; onReconnect: () => void;
  reconnectAvailable: boolean; extraActions?: ReactNode;
  lossless?: { state: "off" | "starting" | "playing"; issue: LosslessStopReason | null; toggle: () => void } | null;
}) {
  const { t, vis } = useCopy();
  const silent = audio.muted || audio.level === 0;
  const percent = Math.round(audio.level * 100);

  return (
    <div ref={controlsRef} className={`lr-playback lr-media-controls${hidden ? " is-hidden" : ""}`} data-can-play={canPlay} role="group" aria-label={t("playback.controls")}>
      <span className="lr-playback-audio">
        <Btn icon={paused ? "play" : "pause"}
          draw="playback-state"
          title={paused ? "playback.play" : "playback.pause"}
          hint={paused ? "hint-local-play" : "hint-local-pause"}
          disabled={!canPlay} onClick={togglePlayback} />
        <span className="lr-playback-separator" aria-hidden="true" />
        <Btn icon={!hasAudio || silent ? "speakerOff" : "speaker"}
          draw="playback-sound"
          title={!hasAudio ? "playback.noAudio" : silent ? "playback.unmute" : "playback.mute"}
          hint={!hasAudio ? "hint-no-audio" : silent ? "hint-unmute" : "hint-mute"} disabled={!hasAudio}
          onClick={toggleMute} />
        <Tooltip kind={!hasAudio ? "hint-no-audio" : audio.boostAvailable ? "hint-volume" : "hint-volume-basic"}
          text={vis ? undefined : t(!hasAudio ? "playback.noAudio" : audio.boostAvailable ? "playback.volume" : "playback.volumeBasic")}
          className="lr-playback-volume">
          <input type="range" min={0} max={audio.boostAvailable ? 200 : 100} step={1}
            value={percent} disabled={!hasAudio}
            aria-label={t(audio.boostAvailable ? "playback.volume" : "playback.volumeBasic")} aria-valuetext={`${percent}%`}
            onChange={(event) => setLevel(Number(event.target.value) / 100)} />
        </Tooltip>
        <span className={`lr-playback-level${percent > 100 ? " is-boosted" : ""}`} aria-hidden="true">
          {hasAudio ? `${percent}%` : "—"}
        </span>
        {lossless ? (
          <Btn icon="audioRepair"
            title={lossless.state === "playing" ? "playback.lossless.on"
              : lossless.state === "starting" ? "playback.lossless.starting"
              : lossless.issue ? "playback.lossless.fallback" : "playback.lossless"}
            hint="hint-volume"
            hintTone={lossless.issue && lossless.state === "off" ? "warn" : undefined}
            busy={lossless.state === "starting"}
            pressed={lossless.state !== "off"} onClick={lossless.toggle} />
        ) : null}
      </span>
      <span className="lr-playback-view">
        {extraActions}
        <Btn icon="refresh" title="viewer.reconnect" hint="hint-reconnect" draw="playback-reconnect"
          disabled={!reconnectAvailable} onClick={onReconnect} />
        <Btn icon={picture.active ? "pipExit" : "pip"} draw="playback-pip"
          hintTone={picture.failed ? "bad" : undefined}
          title={picture.failed ? "playback.pipFailed" : !picture.supported ? "playback.pipUnavailable"
            : picture.active ? "playback.exitPip" : !canPlay ? "playback.pipWaiting" : "playback.pip"}
          hint={!picture.supported || picture.failed || (!picture.active && !canPlay) ? "hint-pip-unavailable" : picture.active ? "hint-pip-exit" : "hint-pip"}
          pressed={picture.active} disabled={!picture.supported || (!picture.active && !canPlay)}
          onClick={() => { void picture.toggle(); }} />
        {!fullscreen.active ? <Btn icon={theaterMode ? "theaterExit" : "theater"}
          draw="playback-theater"
          title={theaterMode ? "viewer.theater.exit" : "viewer.theater"}
          hint={theaterMode ? "hint-theater-exit" : "hint-theater"}
          pressed={theaterMode} onClick={onToggleTheater} /> : null}
        {fullscreen.supported && !theaterMode ? (
          <Btn icon={fullscreen.active ? "contract" : "expand"}
            draw="playback-fullscreen"
            title={fullscreenFailed ? "playback.fullscreenFailed" : fullscreen.active ? "playback.exitFullscreen"
              : !fullscreen.ready ? "playback.fullscreenWaiting" : "playback.fullscreen"}
            hint={fullscreenFailed || (!fullscreen.active && !fullscreen.ready) ? "hint-fullscreen-unavailable"
              : fullscreen.active ? "hint-fullscreen-exit" : "hint-fullscreen"}
            hintTone={fullscreenFailed ? "bad" : !fullscreen.ready ? "warn" : undefined}
            disabled={!fullscreen.active && !fullscreen.ready}
            pressed={fullscreen.active}
            onClick={toggleFullscreen} />
        ) : null}
      </span>
    </div>
  );
}
