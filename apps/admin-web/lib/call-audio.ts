/**
 * Plays the other side of a LiveKit call. livekit-client does NOT play remote audio by itself: each remote audio
 * track has to be attached to an <audio> element in the page. The call screens only attached the avatar's video, so
 * callers heard nothing although the receptionist's audio was arriving.
 */
import type { Room } from "livekit-client";

type LiveKit = typeof import("livekit-client");

export function playRemoteAudio(room: Room, lk: Pick<LiveKit, "RoomEvent" | "Track">): void {
  const elements = new Set<HTMLMediaElement>();
  room.on(lk.RoomEvent.TrackSubscribed, (track) => {
    if (track.kind !== lk.Track.Kind.Audio) return;
    const el = track.attach();
    el.setAttribute("data-agora-call-audio", "");
    el.style.display = "none";
    document.body.appendChild(el);
    elements.add(el);
  });
  room.on(lk.RoomEvent.TrackUnsubscribed, (track) => {
    if (track.kind !== lk.Track.Kind.Audio) return;
    for (const el of track.detach()) {
      el.remove();
      elements.delete(el);
    }
  });
  room.on(lk.RoomEvent.Disconnected, () => {
    for (const el of elements) el.remove();
    elements.clear();
  });
}

/** Browsers may block audio that starts without a user gesture; the call was started by a click, so ask now. */
export async function unlockCallAudio(room: Room): Promise<void> {
  try {
    await room.startAudio();
  } catch {
    // Still blocked: LiveKit retries on the next user interaction (AudioPlaybackStatusChanged).
  }
}
