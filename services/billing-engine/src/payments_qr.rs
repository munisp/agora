//! QR payments (SPEC-W7 B3): Paystack initialize + webhook signature check,
//! static EMVCo-style fallback payloads, and SVG QR rendering.
//!
//! Crypto note: the dependency budget is fixed (payments-service set + sqlx +
//! qrcode only), so HMAC-SHA512 is implemented in-crate below. It is a
//! textbook implementation covered by RFC 4231 and Paystack known-vector
//! unit tests; it is used ONLY for webhook signature verification, never for
//! password storage or key derivation.

use serde::{Deserialize, Serialize};

// ---------------------------------------------------------------------------
// SHA-512 (FIPS 180-4) — pure Rust, no external deps.
// ---------------------------------------------------------------------------

#[rustfmt::skip]
const K: [u64; 80] = [
    0x428a2f98d728ae22, 0x7137449123ef65cd, 0xb5c0fbcfec4d3b2f, 0xe9b5dba58189dbbc,
    0x3956c25bf348b538, 0x59f111f1b605d019, 0x923f82a4af194f9b, 0xab1c5ed5da6d8118,
    0xd807aa98a3030242, 0x12835b0145706fbe, 0x243185be4ee4b28c, 0x550c7dc3d5ffb4e2,
    0x72be5d74f27b896f, 0x80deb1fe3b1696b1, 0x9bdc06a725c71235, 0xc19bf174cf692694,
    0xe49b69c19ef14ad2, 0xefbe4786384f25e3, 0x0fc19dc68b8cd5b5, 0x240ca1cc77ac9c65,
    0x2de92c6f592b0275, 0x4a7484aa6ea6e483, 0x5cb0a9dcbd41fbd4, 0x76f988da831153b5,
    0x983e5152ee66dfab, 0xa831c66d2db43210, 0xb00327c898fb213f, 0xbf597fc7beef0ee4,
    0xc6e00bf33da88fc2, 0xd5a79147930aa725, 0x06ca6351e003826f, 0x142929670a0e6e70,
    0x27b70a8546d22ffc, 0x2e1b21385c26c926, 0x4d2c6dfc5ac42aed, 0x53380d139d95b3df,
    0x650a73548baf63de, 0x766a0abb3c77b2a8, 0x81c2c92e47edaee6, 0x92722c851482353b,
    0xa2bfe8a14cf10364, 0xa81a664bbc423001, 0xc24b8b70d0f89791, 0xc76c51a30654be30,
    0xd192e819d6ef5218, 0xd69906245565a910, 0xf40e35855771202a, 0x106aa07032bbd1b8,
    0x19a4c116b8d2d0c8, 0x1e376c085141ab53, 0x2748774cdf8eeb99, 0x34b0bcb5e19b48a8,
    0x391c0cb3c5c95a63, 0x4ed8aa4ae3418acb, 0x5b9cca4f7763e373, 0x682e6ff3d6b2b8a3,
    0x748f82ee5defb2fc, 0x78a5636f43172f60, 0x84c87814a1f0ab72, 0x8cc702081a6439ec,
    0x90befffa23631e28, 0xa4506cebde82bde9, 0xbef9a3f7b2c67915, 0xc67178f2e372532b,
    0xca273eceea26619c, 0xd186b8c721c0c207, 0xeada7dd6cde0eb1e, 0xf57d4f7fee6ed178,
    0x06f067aa72176fba, 0x0a637dc5a2c898a6, 0x113f9804bef90dae, 0x1b710b35131c471b,
    0x28db77f523047d84, 0x32caab7b40c72493, 0x3c9ebe0a15c9bebc, 0x431d67c49c100d4c,
    0x4cc5d4becb3e42b6, 0x597f299cfc657e2a, 0x5fcb6fab3ad6faec, 0x6c44198c4a475817,
];

pub fn sha512(data: &[u8]) -> [u8; 64] {
    let mut h: [u64; 8] = [
        0x6a09e667f3bcc908, 0xbb67ae8584caa73b, 0x3c6ef372fe94f82b, 0xa54ff53a5f1d36f1,
        0x510e527fade682d1, 0x9b05688c2b3e6c1f, 0x1f83d9abfb41bd6b, 0x5be0cd19137e2179,
    ];

    let bit_len = (data.len() as u128).wrapping_mul(8);
    let mut msg = data.to_vec();
    msg.push(0x80);
    while msg.len() % 128 != 112 {
        msg.push(0);
    }
    msg.extend_from_slice(&bit_len.to_be_bytes());

    for chunk in msg.chunks_exact(128) {
        let mut w = [0u64; 80];
        for (i, word) in w.iter_mut().take(16).enumerate() {
            let mut b = [0u8; 8];
            b.copy_from_slice(&chunk[i * 8..i * 8 + 8]);
            *word = u64::from_be_bytes(b);
        }
        for i in 16..80 {
            let s0 = w[i - 15].rotate_right(1) ^ w[i - 15].rotate_right(8) ^ (w[i - 15] >> 7);
            let s1 = w[i - 2].rotate_right(19) ^ w[i - 2].rotate_right(61) ^ (w[i - 2] >> 6);
            w[i] = w[i - 16]
                .wrapping_add(s0)
                .wrapping_add(w[i - 7])
                .wrapping_add(s1);
        }

        let (mut a, mut b, mut c, mut d, mut e, mut f, mut g, mut hh) =
            (h[0], h[1], h[2], h[3], h[4], h[5], h[6], h[7]);
        for i in 0..80 {
            let s1 = e.rotate_right(14) ^ e.rotate_right(18) ^ e.rotate_right(41);
            let ch = (e & f) ^ ((!e) & g);
            let t1 = hh
                .wrapping_add(s1)
                .wrapping_add(ch)
                .wrapping_add(K[i])
                .wrapping_add(w[i]);
            let s0 = a.rotate_right(28) ^ a.rotate_right(34) ^ a.rotate_right(39);
            let maj = (a & b) ^ (a & c) ^ (b & c);
            let t2 = s0.wrapping_add(maj);
            hh = g;
            g = f;
            f = e;
            e = d.wrapping_add(t1);
            d = c;
            c = b;
            b = a;
            a = t1.wrapping_add(t2);
        }
        h[0] = h[0].wrapping_add(a);
        h[1] = h[1].wrapping_add(b);
        h[2] = h[2].wrapping_add(c);
        h[3] = h[3].wrapping_add(d);
        h[4] = h[4].wrapping_add(e);
        h[5] = h[5].wrapping_add(f);
        h[6] = h[6].wrapping_add(g);
        h[7] = h[7].wrapping_add(hh);
    }

    let mut out = [0u8; 64];
    for (i, v) in h.iter().enumerate() {
        out[i * 8..i * 8 + 8].copy_from_slice(&v.to_be_bytes());
    }
    out
}

fn hex_lower(bytes: &[u8]) -> String {
    let mut s = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        s.push_str(&format!("{b:02x}"));
    }
    s
}

// ---------------------------------------------------------------------------
// HMAC-SHA512 (RFC 2104) + constant-time compare.
// ---------------------------------------------------------------------------

const SHA512_BLOCK: usize = 128;

pub fn hmac_sha512(key: &[u8], msg: &[u8]) -> [u8; 64] {
    let mut k = [0u8; SHA512_BLOCK];
    if key.len() > SHA512_BLOCK {
        let d = sha512(key);
        k[..64].copy_from_slice(&d);
    } else {
        k[..key.len()].copy_from_slice(key);
    }

    let mut inner = Vec::with_capacity(SHA512_BLOCK + msg.len());
    for b in k.iter() {
        inner.push(b ^ 0x36);
    }
    inner.extend_from_slice(msg);
    let inner_hash = sha512(&inner);

    let mut outer = Vec::with_capacity(SHA512_BLOCK + 64);
    for b in k.iter() {
        outer.push(b ^ 0x5c);
    }
    outer.extend_from_slice(&inner_hash);
    sha512(&outer)
}

/// Length-independent XOR fold (both operands are fixed-size hex digests in
/// practice; length mismatch still fails fast without early-exit on content).
pub fn constant_time_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for (x, y) in a.iter().zip(b.iter()) {
        diff |= x ^ y;
    }
    diff == 0
}

/// Verify a Paystack `x-paystack-signature` header against the raw request
/// body (B3): HMAC-SHA512 hex of body under the secret key, compared in
/// constant time.
pub fn verify_paystack_signature(secret: &str, body: &[u8], signature: &str) -> bool {
    let expected = hex_lower(&hmac_sha512(secret.as_bytes(), body));
    constant_time_eq(expected.as_bytes(), signature.trim().as_bytes())
}

// ---------------------------------------------------------------------------
// CRC16-CCITT (init 0xFFFF, poly 0x1021, no reflection) for EMV payloads.
// ---------------------------------------------------------------------------

pub fn crc16_ccitt(data: &[u8]) -> u16 {
    let mut crc: u16 = 0xFFFF;
    for &b in data {
        crc ^= (b as u16) << 8;
        for _ in 0..8 {
            crc = if crc & 0x8000 != 0 {
                (crc << 1) ^ 0x1021
            } else {
                crc << 1
            };
        }
    }
    crc
}

// ---------------------------------------------------------------------------
// Static EMVCo-style merchant payload (fallback when no PAYSTACK_SECRET_KEY).
// ---------------------------------------------------------------------------

fn tlv(tag: &str, value: &str) -> String {
    format!("{}{:02}{}", tag, value.len(), value)
}

/// ISO 4217 numeric currency codes for the currencies we see in practice.
fn currency_numeric(code: &str) -> &'static str {
    match code.to_ascii_uppercase().as_str() {
        "NGN" => "566",
        "USD" => "840",
        "GHS" => "936",
        "KES" => "404",
        "ZAR" => "710",
        "EUR" => "978",
        "GBP" => "826",
        _ => "999", // XXX (no currency)
    }
}

/// Build an EMVCo-like merchant-presented payload. Layout mirrors the EMV QRC
/// merchant mode: 00 format indicator, 01 static (02), 26 merchant account
/// template (00 GUI "OPENDESK", 01 account), 52 MCC, 53 currency, 54 amount
/// in major units, 58 country, 59 merchant name, 62 additional data (05
/// reference), 63 CRC16.
pub fn build_static_payload(
    merchant_name: &str,
    account: &str,
    amount_cents: i64,
    currency: &str,
    reference: &str,
) -> String {
    let amount = format!("{}.{:02}", amount_cents / 100, (amount_cents % 100).abs());
    let merchant_account = tlv("00", "OPENDESK") + &tlv("01", account);
    let mut payload = String::new();
    payload.push_str(&tlv("00", "01"));
    payload.push_str(&tlv("01", "02"));
    payload.push_str(&tlv("26", &merchant_account));
    payload.push_str(&tlv("52", "0000"));
    payload.push_str(&tlv("53", currency_numeric(currency)));
    payload.push_str(&tlv("54", &amount));
    payload.push_str(&tlv("58", "NG"));
    payload.push_str(&tlv("59", merchant_name));
    payload.push_str(&tlv("62", &tlv("05", reference)));
    payload.push_str("6304");
    let crc = crc16_ccitt(payload.as_bytes());
    format!("{}{:04X}", payload, crc)
}

// ---------------------------------------------------------------------------
// SVG QR rendering (qrcode crate, svg feature).
// ---------------------------------------------------------------------------

pub fn qr_svg(data: &str) -> Result<String, String> {
    let code = qrcode::QrCode::new(data.as_bytes()).map_err(|e| e.to_string())?;
    Ok(code
        .render::<qrcode::render::svg::Color>()
        .min_dimensions(200, 200)
        .build())
}

// ---------------------------------------------------------------------------
// Paystack initialize API (B3).
// ---------------------------------------------------------------------------

#[derive(Debug, Serialize)]
pub struct PaystackInitRequest {
    pub email: String,
    /// Amount in kobo (minor units).
    pub amount: i64,
    pub reference: String,
    pub callback_url: String,
    pub metadata: serde_json::Value,
}

#[derive(Debug, Deserialize)]
struct PaystackInitData {
    authorization_url: String,
    reference: String,
}

#[derive(Debug, Deserialize)]
struct PaystackInitResponse {
    status: bool,
    #[serde(default)]
    message: String,
    data: Option<PaystackInitData>,
}

pub async fn paystack_initialize(
    http: &reqwest::Client,
    secret: &str,
    req: &PaystackInitRequest,
) -> Result<(String, String), String> {
    let resp = http
        .post("https://api.paystack.co/transaction/initialize")
        .bearer_auth(secret)
        .json(req)
        .send()
        .await
        .map_err(|e| format!("paystack initialize request failed: {e}"))?;
    if !resp.status().is_success() {
        let status = resp.status();
        let body = resp.text().await.unwrap_or_default();
        return Err(format!("paystack initialize HTTP {status}: {body}"));
    }
    let parsed: PaystackInitResponse = resp
        .json()
        .await
        .map_err(|e| format!("paystack initialize decode: {e}"))?;
    if !parsed.status {
        return Err(format!("paystack initialize rejected: {}", parsed.message));
    }
    let data = parsed
        .data
        .ok_or_else(|| "paystack initialize: missing data".to_string())?;
    Ok((data.authorization_url, data.reference))
}

// ---------------------------------------------------------------------------
// Unit tests: SHA-512 / HMAC known vectors, CRC16, EMV payload known vector.
// ---------------------------------------------------------------------------
#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sha512_matches_fips_vector() {
        // FIPS 180-4 example: SHA-512("abc").
        let d = sha512(b"abc");
        assert_eq!(
            hex_lower(&d),
            "ddaf35a193617abacc417349ae20413112e6fa4e89a97ea20a9eeee64b55d39a\
             2192992a274fc1a836ba3c23a3feebbd454d4423643ce80e2a9ac94fa54ca49f"
                .replace(' ', "")
        );
        // Empty input vector.
        assert_eq!(
            hex_lower(&sha512(b"")),
            "cf83e1357eefb8bdf1542850d66d8007d620e4050b5715dc83f4a921d36ce9ce\
             47d0d13c5d85f2b0ff8318d2877eec2f63b931bd47417a81a538327af927da3e"
                .replace(' ', "")
        );
    }

    #[test]
    fn hmac_sha512_matches_rfc4231_case1() {
        // RFC 4231 test case 1: key = 20 x 0x0b, data = "Hi There".
        let key = [0x0bu8; 20];
        let out = hmac_sha512(&key, b"Hi There");
        assert_eq!(
            hex_lower(&out),
            "87aa7cdea5ef619d4ff0b4241a1d6cb02379f4e2ce4ec2787ad0b30545e17cde\
             daa833b7d6b8a702038b274eaea3f4e4be9d914eeb61f1702e696c203a126854"
                .replace(' ', "")
        );
    }

    #[test]
    fn paystack_signature_known_vector() {
        // Vector computed with python3 hmac (see docs/billing.md).
        let secret = "sk_test_0123456789abcdef0123456789abcdef";
        let body = br#"{"event":"charge.success","data":{"reference":"9b0b0d52-1c8b-4d3f-9e2a-6f6a2b7c1d20","amount":125000,"currency":"NGN","status":"success"}}"#;
        let expected = "683c86c3dad9b20fe26cd1e35511d249972a63057b42b562b318161a322e9161b4628d7502f911e231c3e4e5b6029f0fd0be674cd633179525979ee365bb7101";
        assert!(verify_paystack_signature(secret, body, expected));
        // Tampered body and wrong signature must both fail.
        assert!(!verify_paystack_signature(secret, b"{}", expected));
        assert!(!verify_paystack_signature(
            secret,
            body,
            "000086c3dad9b20fe26cd1e35511d249972a63057b42b562b318161a322e9161b4628d7502f911e231c3e4e5b6029f0fd0be674cd633179525979ee365bb7101"
        ));
    }

    #[test]
    fn constant_time_eq_behaviour() {
        assert!(constant_time_eq(b"abc", b"abc"));
        assert!(!constant_time_eq(b"abc", b"abd"));
        assert!(!constant_time_eq(b"abc", b"ab"));
        assert!(constant_time_eq(b"", b""));
    }

    #[test]
    fn crc16_ccitt_check_value() {
        // Classic CRC-16/CCITT-FALSE check value.
        assert_eq!(crc16_ccitt(b"123456789"), 0x29B1);
    }

    #[test]
    fn static_payload_matches_emv_known_vector() {
        // Vector computed with python3 (same field layout); see docs/billing.md.
        let p = build_static_payload(
            "OPENDESK DEMO",
            "OPENDESK/0123456789",
            125_000,
            "NGN",
            "9b0b0d52-1c8b-4d3f-9e2a-6f6a2b7c1d20",
        );
        // NOTE: the space in the merchant name "OPENDESK DEMO" is a legitimate
        // payload byte (tag 59, length 13) — Rust's `\`-continuation already
        // strips the source line breaks/indentation, so no `.replace(' ',"")`
        // here (that used to nuke the merchant-name space and fail the test).
        assert_eq!(
            p,
            "00020101020226350008OPENDESK0119OPENDESK/0123456789520400005303566540\
             71250.005802NG5913OPENDESK DEMO624005369b0b0d52-1c8b-4d3f-9e2a-6f6a\
             2b7c1d2063046EBD"
        );
        // Payload must end with its own CRC16.
        let (body, crc) = p.split_at(p.len() - 4);
        assert_eq!(format!("{:04X}", crc16_ccitt(body.as_bytes())), crc);
    }

    #[test]
    fn static_payload_amount_formatting() {
        let p = build_static_payload("M", "A/1", 5, "NGN", "r");
        assert!(p.contains("54040.05"), "kobo/cents render as major units: {p}");
        let p = build_static_payload("M", "A/1", 99, "USD", "r");
        assert!(p.contains("54040.99"), "cents pad to two digits: {p}");
    }

    #[test]
    fn qr_svg_renders_svg_markup() {
        let svg = qr_svg("0002010102").unwrap();
        assert!(svg.contains("<svg"), "expected svg markup, got: {svg}");
    }
}
