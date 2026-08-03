/**
 * Push registration (SPEC-W16 cross-agent contract §1).
 *
 * Flow: request OS permission → getExpoPushTokenAsync → POST /v1/devices
 * {token, platform: "android"|"ios", app: "field"} on booking-service
 * (Agent B owns the endpoint; this client codes to the contract). On
 * sign-out we DELETE /v1/devices/{token} so the tenant stops receiving
 * pushes on a signed-out device.
 *
 * Server-side fan-out (FCM now, APNs stub) lives in notification-worker —
 * see docs/push-notifications.md (Agent A). Expo push tokens are accepted
 * by FCM via Expo's push service; a native FCM token swap is a documented
 * follow-up (README "Push notifications").
 */
import { Platform } from "react-native";
import * as Notifications from "expo-notifications";
import { registerDevice, deleteDevice } from "../api/client";
import {
  getRegisteredDeviceToken,
  setRegisteredDeviceToken,
} from "../auth/session";

// Foreground presentation: show banner + sound instead of swallowing.
Notifications.setNotificationHandler({
  handleNotification: async () => ({
    shouldShowAlert: true,
    shouldPlaySound: true,
    shouldSetBadge: false,
  }),
});

function devicePlatform(): "android" | "ios" | "web" {
  if (Platform.OS === "ios") return "ios";
  if (Platform.OS === "android") return "android";
  return "web";
}

/**
 * Request permission, obtain the Expo push token and register it with the
 * BFF. Returns the token, or null when permission is denied / running on a
 * simulator without push support. Never throws — push must not block the
 * rest of sign-in.
 */
export async function registerForPushNotifications(): Promise<string | null> {
  try {
    const existing = await Notifications.getPermissionsAsync();
    let status = existing.status;
    if (status !== "granted") {
      const req = await Notifications.requestPermissionsAsync();
      status = req.status;
    }
    if (status !== "granted") return null;

    if (Platform.OS === "android") {
      await Notifications.setNotificationChannelAsync("default", {
        name: "Default",
        importance: Notifications.AndroidImportance.DEFAULT,
      });
    }

    const pushToken = await Notifications.getExpoPushTokenAsync();
    const token = pushToken.data;
    if (!token) return null;

    // Skip the network call if we already registered this exact token.
    const already = await getRegisteredDeviceToken();
    if (already === token) return token;

    await registerDevice({
      token,
      platform: devicePlatform(),
      app: "field",
    });
    await setRegisteredDeviceToken(token);
    return token;
  } catch {
    // Simulator / no Google Play services / offline — documented limitation.
    return null;
  }
}

/** Unregister on sign-out (best-effort, never throws). */
export async function unregisterPushNotifications(): Promise<void> {
  try {
    const token = await getRegisteredDeviceToken();
    if (token) {
      await deleteDevice(token);
      await setRegisteredDeviceToken(null);
    }
  } catch {
    // best-effort: the row's last_seen_at ages out server-side
  }
}
