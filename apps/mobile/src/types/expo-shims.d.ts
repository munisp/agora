/**
 * Minimal TypeScript module shims for Expo / React Native imports.
 *
 * WHY: the repo gate is `npx tsc --noEmit` WITHOUT installing the Expo
 * dependency tree (SPEC-W16 Agent D constraints). These declarations cover
 * exactly the surface this app uses — no more.
 *
 * With real node_modules present the package types resolve first and
 * these shims are inert (verified: `tsc --noEmit` passes both ways on
 * SDK 51). You may delete this file once you develop against a full
 * install — see README "Typechecking without Expo".
 *
 * Keep the shims honest: they describe the real SDK 51 API shapes, just
 * narrowed. If you add a new expo package / react-native import, extend
 * this file.
 */
type ReactComponentType<P> = import("react").ComponentType<P>;
type ReactElement = import("react").ReactElement;

declare module "react-native" {
  export type StyleProp<T> = T | null | undefined | false | StyleProp<T>[];
  export interface ViewStyle {
    [key: string]: unknown;
  }
  export interface TextStyle {
    [key: string]: unknown;
  }
  export type ColorValue = string;
  /** Real type is a union of RN keyboard names; string is a safe superset. */
  export type KeyboardTypeOptions = string;

  export const View: ReactComponentType<Record<string, unknown> & { style?: StyleProp<ViewStyle> }>;
  export const Text: ReactComponentType<Record<string, unknown> & { style?: StyleProp<TextStyle> }>;
  export const TextInput: ReactComponentType<
    Record<string, unknown> & {
      value?: string;
      onChangeText?: (text: string) => void;
      placeholder?: string;
      style?: StyleProp<TextStyle>;
    }
  >;
  export const ScrollView: ReactComponentType<Record<string, unknown> & { style?: StyleProp<ViewStyle> }>;
  export const Pressable: ReactComponentType<
    Record<string, unknown> & {
      onPress?: () => void;
      disabled?: boolean;
      style?: StyleProp<ViewStyle> | ((state: { pressed: boolean }) => StyleProp<ViewStyle>);
    }
  >;
  export const ActivityIndicator: ReactComponentType<{ size?: "small" | "large"; color?: string } & Record<string, unknown>>;
  export const RefreshControl: ReactComponentType<{ refreshing: boolean; onRefresh?: () => void; tintColor?: string } & Record<string, unknown>>;
  export const KeyboardAvoidingView: ReactComponentType<{ behavior?: "height" | "position" | "padding"; style?: StyleProp<ViewStyle> } & Record<string, unknown>>;
  export const Modal: ReactComponentType<{ visible?: boolean; transparent?: boolean; animationType?: "none" | "slide" | "fade" } & Record<string, unknown>>;
  export const Switch: ReactComponentType<{ value?: boolean; onValueChange?: (v: boolean) => void } & Record<string, unknown>>;
  export const SafeAreaView: ReactComponentType<Record<string, unknown> & { style?: StyleProp<ViewStyle> }>;

  export interface FlatListProps<ItemT> {
    data?: readonly ItemT[] | null;
    renderItem?: (info: { item: ItemT; index: number }) => ReactElement | null;
    keyExtractor?: (item: ItemT, index: number) => string;
    refreshControl?: ReactElement;
    ListEmptyComponent?: React.ComponentType | ReactElement;
    contentContainerStyle?: StyleProp<ViewStyle>;
    style?: StyleProp<ViewStyle>;
    [key: string]: unknown;
  }
  export function FlatList<ItemT = unknown>(props: FlatListProps<ItemT>): ReactElement | null;

  export const StyleSheet: {
    create<T extends { [name: string]: ViewStyle | TextStyle }>(styles: T): T;
    hairlineWidth: number;
    absoluteFillObject: ViewStyle;
  };

  export const Platform: {
    OS: "ios" | "android" | "windows" | "macos" | "web";
    Version: number | string;
    select<T>(spec: { ios?: T; android?: T; web?: T; default?: T }): T | undefined;
  };

  export interface AlertButton {
    text?: string;
    onPress?: () => void;
    style?: "default" | "cancel" | "destructive";
  }
  export const Alert: {
    alert(title: string, message?: string, buttons?: AlertButton[]): void;
  };
}

declare module "expo-constants" {
  export interface ExpoConfig {
    name?: string;
    slug?: string;
    scheme?: string | string[];
    version?: string;
    extra?: Record<string, unknown>;
    [key: string]: unknown;
  }
  const Constants: {
    expoConfig?: ExpoConfig | null;
    manifest?: unknown;
    platform?: { ios?: unknown; android?: unknown };
    [key: string]: unknown;
  };
  export default Constants;
}

declare module "expo-secure-store" {
  export function getItemAsync(key: string): Promise<string | null>;
  export function setItemAsync(key: string, value: string): Promise<void>;
  export function deleteItemAsync(key: string): Promise<void>;
}

declare module "expo-notifications" {
  export type PermissionStatus = "granted" | "denied" | "undetermined";
  export interface NotificationPermissionsStatus {
    status: PermissionStatus;
    granted: boolean;
    canAskAgain: boolean;
  }
  export function getPermissionsAsync(): Promise<NotificationPermissionsStatus>;
  export function requestPermissionsAsync(): Promise<NotificationPermissionsStatus>;

  export interface ExpoPushToken {
    type: "expo";
    data: string;
  }
  export function getExpoPushTokenAsync(options?: {
    projectId?: string;
  }): Promise<ExpoPushToken>;

  export enum AndroidImportance {
    DEFAULT = 3,
    HIGH = 4,
    MAX = 5,
    LOW = 2,
    MIN = 1,
  }
  export interface NotificationChannelInput {
    name: string;
    importance: AndroidImportance;
    [key: string]: unknown;
  }
  export function setNotificationChannelAsync(
    channelId: string,
    channel: NotificationChannelInput,
  ): Promise<unknown>;

  export interface NotificationBehavior {
    shouldShowAlert: boolean;
    shouldPlaySound: boolean;
    shouldSetBadge: boolean;
  }
  export function setNotificationHandler(handler: {
    handleNotification: () => Promise<NotificationBehavior>;
  }): void;
}

declare module "expo-auth-session" {
  export enum ResponseType {
    Code = "code",
    Token = "token",
  }
  export enum CodeChallengeMethod {
    Plain = "plain",
    S256 = "S256",
  }

  export interface DiscoveryDocument {
    authorizationEndpoint?: string;
    tokenEndpoint?: string;
    revocationEndpoint?: string;
    endSessionEndpoint?: string;
    issuer?: string;
    [key: string]: unknown;
  }
  export function fetchDiscoveryAsync(issuerUrl: string): Promise<DiscoveryDocument>;

  export function makeRedirectUri(options?: {
    scheme?: string;
    path?: string;
    native?: string;
    isTripleSlashed?: boolean;
  }): string;

  export interface AuthRequestConfig {
    clientId: string;
    redirectUri?: string;
    scopes?: string[];
    responseType?: ResponseType;
    usePKCE?: boolean;
    codeChallengeMethod?: CodeChallengeMethod;
    state?: string;
  }

  export type AuthSessionResult =
    | { type: "success"; params: { code: string; state?: string }; url: string }
    | { type: "cancel" | "dismiss" }
    | { type: "error"; error?: unknown; params?: Record<string, string> };

  export class AuthRequest {
    constructor(config: AuthRequestConfig);
    readonly codeVerifier: string;
    redirectUri: string;
    promptAsync(
      discovery: DiscoveryDocument,
      options?: Record<string, unknown>,
    ): Promise<AuthSessionResult>;
  }

  export class TokenResponse {
    accessToken: string;
    refreshToken?: string;
    idToken?: string;
    expiresIn?: number;
    tokenType?: string;
    scope?: string;
  }

  export interface AccessTokenRequestConfig {
    clientId: string;
    code: string;
    redirectUri?: string;
    scopes?: string[];
    extraParams?: Record<string, string>;
  }
  export function exchangeCodeAsync(
    config: AccessTokenRequestConfig,
    discovery: DiscoveryDocument,
  ): Promise<TokenResponse>;

  export function refreshAsync(
    config: { clientId: string; refreshToken: string; scopes?: string[] },
    discovery: DiscoveryDocument,
  ): Promise<TokenResponse>;

  export function revokeAsync(
    config: { clientId: string; token: string; tokenTypeHint?: string },
    discovery: DiscoveryDocument,
  ): Promise<boolean>;
}

declare module "expo-web-browser" {
  export function maybeCompleteAuthSession(): void;
}

declare module "expo-router" {
  export const Stack: ReactComponentType<Record<string, unknown>> & {
    Screen: ReactComponentType<Record<string, unknown>>;
  };
  export const Tabs: ReactComponentType<Record<string, unknown>> & {
    Screen: ReactComponentType<Record<string, unknown>>;
  };
  export const Redirect: ReactComponentType<{ href: string }>;
  export const Link: ReactComponentType<
    { href: string; asChild?: boolean } & Record<string, unknown>
  >;

  export interface Router {
    push(href: string): void;
    replace(href: string): void;
    navigate(href: string): void;
    back(): void;
    canGoBack(): boolean;
  }
  export function useRouter(): Router;
  export const router: Router;

  export function useLocalSearchParams<
    T extends Record<string, string | string[] | undefined> = Record<
      string,
      string | string[] | undefined
    >,
  >(): T;

  /** Runs the effect each time the screen gains focus; return a cleanup. */
  export function useFocusEffect(effect: () => void | (() => void)): void;
}

declare module "expo-status-bar" {
  export const StatusBar: ReactComponentType<{
    style?: "auto" | "inverted" | "light" | "dark";
    hidden?: boolean;
  }>;
}

declare module "react-native-safe-area-context" {
  export interface EdgeInsets {
    top: number;
    right: number;
    bottom: number;
    left: number;
  }
  export const SafeAreaProvider: ReactComponentType<Record<string, unknown>>;
  export const SafeAreaView: ReactComponentType<Record<string, unknown>>;
  export function useSafeAreaInsets(): EdgeInsets;
}
