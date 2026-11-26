#pragma once

#include <array>
#include <cinttypes>
#include <cmath>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <utility>
#include "brd_rtc.h"

#ifndef RTC_DATA_ATTR
#define RTC_DATA_ATTR
#endif

namespace brd_power {

constexpr uint32_t RTC_STATE_MAGIC = 0x42524450;
constexpr uint32_t RTC_SYNC_INTERVAL_SEC = 21600U;

// Bit constants for the 74HCT4094D shift register.
// CFG_RAILS_ONLY_ON sets ep_scan_direction=1 and power_disable=1.
// CFG_ALL_OFF_SMPS_DISABLED clears ep_scan_direction and sets power_disable=1.
constexpr uint8_t CFG_RAILS_ONLY_ON = 0x22;
constexpr uint8_t CFG_ALL_OFF_SMPS_DISABLED = 0x02;

// Piecewise linear discharge curve for 1S lithium polymer cells.
// The array matches the ten points from the battery driver table.
static constexpr std::array<std::pair<float, float>, 10> VOLTAGE_CURVE = {{
    {4.20f, 100.0f},
    {4.10f, 92.0f},
    {4.00f, 83.0f},
    {3.90f, 72.0f},
    {3.80f, 57.0f},
    {3.70f, 41.0f},
    {3.60f, 24.0f},
    {3.50f, 13.0f},
    {3.30f, 4.0f},
    {3.00f, 0.0f},
}};

inline float battery_voltage_to_percent(float voltage, float min_voltage = 3.00f,
                                       float max_voltage = 4.20f) {
  if (!std::isfinite(voltage)) {
    return 0.0f;
  }
  if (voltage >= max_voltage) {
    return 100.0f;
  }
  if (voltage <= min_voltage) {
    return 0.0f;
  }

  if (voltage >= VOLTAGE_CURVE.front().first) {
    return VOLTAGE_CURVE.front().second;
  }
  if (voltage <= VOLTAGE_CURVE.back().first) {
    return VOLTAGE_CURVE.back().second;
  }

  for (size_t i = 0; i + 1 < VOLTAGE_CURVE.size(); ++i) {
    const float v_hi = VOLTAGE_CURVE[i].first;
    const float v_lo = VOLTAGE_CURVE[i + 1].first;
    if (voltage >= v_lo && voltage <= v_hi) {
      const float p_hi = VOLTAGE_CURVE[i].second;
      const float p_lo = VOLTAGE_CURVE[i + 1].second;
      const float t = (voltage - v_lo) / (v_hi - v_lo);
      return p_lo + t * (p_hi - p_lo);
    }
  }
  return 0.0f;
}

inline bool is_valid_touch_point(int32_t x, int32_t y, int32_t width, int32_t height) {
  return width > 0 && height > 0 && x >= 0 && x < width && y >= 0 && y < height;
}

struct RtcPersistentState {
  uint32_t magic{RTC_STATE_MAGIC};
  uint32_t boot_count{0};
  uint32_t prev_trace_epoch{0};
  bool prev_slept{false};
  int8_t prev_gt911{-1};
  int8_t last_gt911_addr{0};
  uint32_t last_rtc_sync_epoch{0};
  uint32_t prev_batt_mv{0};
  bool staying_awake_usb{false};
  char last_img_hash[65]{};
  uint32_t crc32{0};

  uint32_t calculate_crc() const {
    const uint8_t *data = reinterpret_cast<const uint8_t *>(this);
    const size_t len = offsetof(RtcPersistentState, crc32);
    uint32_t crc = 0xFFFFFFFFU;
    for (size_t i = 0; i < len; ++i) {
      crc ^= data[i];
      for (int b = 0; b < 8; ++b) {
        crc = (crc >> 1) ^ (0xEDB88320U & (0 - (crc & 1)));
      }
    }
    return ~crc;
  }

  void update_crc() {
    this->crc32 = this->calculate_crc();
  }

  bool validate_or_reset() {
    if (this->magic == RTC_STATE_MAGIC && this->crc32 == this->calculate_crc()) {
      return true;
    }
    this->reset();
    return false;
  }

  void reset() {
    this->magic = RTC_STATE_MAGIC;
    this->boot_count = 0;
    this->prev_trace_epoch = 0;
    this->prev_slept = false;
    this->prev_gt911 = -1;
    this->last_gt911_addr = 0;
    this->last_rtc_sync_epoch = 0;
    this->prev_batt_mv = 0;
    this->staying_awake_usb = false;
    for (size_t i = 0; i < sizeof(this->last_img_hash); ++i) {
      this->last_img_hash[i] = '\0';
    }
    this->update_crc();
  }
};

inline RTC_DATA_ATTR RtcPersistentState rtc_state;

inline uint32_t sleep_lock_count{0};

inline void acquire_sleep_lock() {
  ++sleep_lock_count;
}

inline void release_sleep_lock() {
  if (sleep_lock_count > 0U) {
    --sleep_lock_count;
  }
}

inline bool is_sleep_locked() {
  return sleep_lock_count > 0U;
}

inline bool can_enter_sleep(bool staying_awake_usb, bool image_task_running,
                            bool epd_refresh_active, uint32_t active_sleep_locks) {
  return !staying_awake_usb && !image_task_running && !epd_refresh_active &&
         active_sleep_locks == 0U;
}

inline uint32_t voltage_to_mv(float v) {
  if (std::isnan(v) || v <= 0.0f || v > 10.0f) {
    return 0U;
  }
  return static_cast<uint32_t>(std::lround(v * 1000.0f));
}

inline const char *classify_wake_reason(int wake_cause, uint64_t ext1_mask, bool rtc_af,
                                       bool rtc_tf) {
  // Handle EXT1 wakeup triggers.
  if (wake_cause == 3) {
    if ((ext1_mask & (1ULL << 21)) != 0) {
      return "Ext_BTN";
    }
    if ((ext1_mask & (1ULL << 9)) != 0) {
      return "Ext_RTC";
    }
    if (rtc_af || rtc_tf) {
      return "Ext_RTC";
    }
    return "Ext_RTC";
  }

  // Handle timer wakeup triggers.
  if (wake_cause == 4) {
    return "Int_RTC";
  }

  // Handle initial power activation.
  if (wake_cause == 0) {
    return "Power_On";
  }

  return "Other";
}

inline bool evaluate_usb_power(uint32_t current_mv, uint32_t prev_mv, bool currently_awake_usb,
                               bool usb_sof_incremented, bool prev_slept = false) {
  if (usb_sof_incremented) {
    return true;
  }

  if (currently_awake_usb) {
    return current_mv >= 4185U;
  }

  if (prev_mv > 0 && current_mv >= prev_mv + 25U) {
    return true;
  }

  if (prev_mv == 0 && !prev_slept && current_mv >= 4195U) {
    return true;
  }

  return false;
}

inline bool is_rtc_sync_due(bool rtc_valid, uint32_t current_epoch, uint32_t last_sync_epoch,
                            uint32_t interval_sec = RTC_SYNC_INTERVAL_SEC) {
  if (!rtc_valid || last_sync_epoch == 0U || current_epoch < last_sync_epoch) {
    return true;
  }
  return (current_epoch - last_sync_epoch) >= interval_sec;
}

inline const char *classify_rtc_sync_status(bool sync_requested, bool ntp_synced, bool rtc_saved) {
  if (ntp_synced) {
    return rtc_saved ? "synced" : "save_err";
  }
  if (sync_requested) {
    return "timeout";
  }
  return "skipped";
}

inline int classify_rtc_sync_metric(bool sync_requested, bool ntp_synced, bool rtc_saved) {
  if (ntp_synced) {
    return rtc_saved ? 1 : -1;
  }
  if (sync_requested) {
    return -1;
  }
  return 0;
}

inline long compute_rtc_sync_age(uint32_t now_epoch, uint32_t last_sync_epoch) {
  if (now_epoch == 0U || last_sync_epoch == 0U || now_epoch < last_sync_epoch) {
    return -1;
  }
  return static_cast<long>(now_epoch - last_sync_epoch);
}

struct TraceTelemetry {
  const char *wake_reason{"unknown"};
  const char *mode{"none"};
  float batt_v{0.0f};
  float batt_pct{0.0f};
  float awake_s{0.0f};
  const char *image_status{"cached"};
  int touch_x{-1};
  int touch_y{-1};
  const char *now_str{"unknown"};
  const char *next_str{"unknown"};
  const char *next_in_str{"unknown"};
  const char *since_str{"unknown"};
  bool prev_slept{false};
  int prev_gt911{-1};
  uint32_t boot_count{0};
  const char *job_name{"brd"};
  long since_num{-1};
  const char *rtc_sync{"skipped"};
  int rtc_sync_code{0};
  long rtc_sync_age_sec{-1};
};

inline void escape_prom_label(const char *src, char *dst, size_t dst_len) {
  if (dst == nullptr || dst_len == 0U) {
    return;
  }
  if (src == nullptr) {
    dst[0] = '\0';
    return;
  }
  size_t out = 0U;
  for (size_t i = 0U; src[i] != '\0' && out + 1U < dst_len; ++i) {
    const char c = src[i];
    if (c == '\\' || c == '"' || c == '\n') {
      if (out + 2U >= dst_len) {
        break;
      }
      dst[out++] = '\\';
      dst[out++] = (c == '\n') ? 'n' : c;
    } else if (c == '\r') {
      continue;
    } else {
      dst[out++] = c;
    }
  }
  dst[out] = '\0';
}

inline int format_vlogs_message(char *buf, size_t len, const TraceTelemetry &t) {
  char touch_buf[24];
  if (t.touch_x >= 0 && t.touch_y >= 0) {
    snprintf(touch_buf, sizeof(touch_buf), "%d,%d", t.touch_x, t.touch_y);
  } else {
    snprintf(touch_buf, sizeof(touch_buf), "none");
  }

  const char *gt_str = (t.prev_gt911 == 1)   ? "0x5D"
                       : (t.prev_gt911 == 2) ? "0x14"
                       : (t.prev_gt911 == 0) ? "nack"
                                             : "none";

  char batt_v_buf[16];
  if (std::isfinite(t.batt_v)) {
    snprintf(batt_v_buf, sizeof(batt_v_buf), "%.2fV", t.batt_v);
  } else {
    snprintf(batt_v_buf, sizeof(batt_v_buf), "unknown");
  }

  char batt_pct_buf[16];
  if (std::isfinite(t.batt_pct)) {
    snprintf(batt_pct_buf, sizeof(batt_pct_buf), "%.0f%%", t.batt_pct);
  } else {
    snprintf(batt_pct_buf, sizeof(batt_pct_buf), "unknown");
  }

  const float awake_s = std::isfinite(t.awake_s) ? t.awake_s : 0.0f;

  char sync_age_buf[24];
  if (t.rtc_sync_age_sec >= 0) {
    snprintf(sync_age_buf, sizeof(sync_age_buf), "%lds", t.rtc_sync_age_sec);
  } else {
    snprintf(sync_age_buf, sizeof(sync_age_buf), "unknown");
  }

  return snprintf(buf, len,
                  "wake=%s mode=%s batt=%s batt_pct=%s awake=%.1fs image=%s touch=%s "
                  "now=%s next=%s next_in=%ss since=%ss slept=%s prev_gt911=%s boots=%" PRIu32
                  " rtc_sync=%s rtc_sync_age=%s",
                  t.wake_reason != nullptr ? t.wake_reason : "unknown",
                  t.mode != nullptr ? t.mode : "none",
                  batt_v_buf,
                  batt_pct_buf,
                  awake_s,
                  t.image_status != nullptr ? t.image_status : "cached",
                  touch_buf,
                  t.now_str != nullptr ? t.now_str : "unknown",
                  t.next_str != nullptr ? t.next_str : "unknown",
                  t.next_in_str != nullptr ? t.next_in_str : "unknown",
                  t.since_str != nullptr ? t.since_str : "unknown",
                  t.prev_slept ? "yes" : "no",
                  gt_str,
                  t.boot_count,
                  t.rtc_sync != nullptr ? t.rtc_sync : "skipped",
                  sync_age_buf);
}

inline int format_vmetrics_prometheus(char *buf, size_t len, const TraceTelemetry &t) {
  const int gt911_slept_val = (t.prev_gt911 == 0) ? 0 : ((t.prev_gt911 > 0) ? 1 : -1);
  char job[96];
  escape_prom_label(t.job_name != nullptr ? t.job_name : "brd", job, sizeof(job));

  const float awake_s = std::isfinite(t.awake_s) ? t.awake_s : 0.0f;

  char batt_lines[256];
  int batt_len = 0;
  batt_lines[0] = '\0';
  if (std::isfinite(t.batt_v)) {
    const int n = snprintf(batt_lines + batt_len, sizeof(batt_lines) - static_cast<size_t>(batt_len),
                           "battery_voltage{job=\"%s\"} %.2f\n", job, t.batt_v);
    if (n > 0 && static_cast<size_t>(batt_len + n) < sizeof(batt_lines)) {
      batt_len += n;
    }
  }
  if (std::isfinite(t.batt_pct)) {
    const int n = snprintf(batt_lines + batt_len, sizeof(batt_lines) - static_cast<size_t>(batt_len),
                           "battery_percent{job=\"%s\"} %.0f\n", job, t.batt_pct);
    if (n > 0 && static_cast<size_t>(batt_len + n) < sizeof(batt_lines)) {
      batt_len += n;
    }
  }

  return snprintf(buf, len,
                  "%s"
                  "wake_secs{job=\"%s\"} %.1f\n"
                  "run_gap_secs{job=\"%s\"} %ld\n"
                  "prev_slept{job=\"%s\"} %d\n"
                  "gt911_slept{job=\"%s\"} %d\n"
                  "boot_count{job=\"%s\"} %" PRIu32 "\n"
                  "rtc_synced{job=\"%s\"} %d\n"
                  "rtc_sync_age_secs{job=\"%s\"} %ld",
                  batt_lines,
                  job, awake_s,
                  job, t.since_num,
                  job, t.prev_slept ? 1 : 0,
                  job, gt911_slept_val,
                  job, t.boot_count,
                  job, t.rtc_sync_code,
                  job, t.rtc_sync_age_sec);
}

}  // namespace brd_power

#ifdef USE_ESP32

#include <Arduino.h>
#include <driver/gpio.h>
#include <driver/rtc_io.h>
#include <esp_sleep.h>
#include <esp_sntp.h>
#include <esp_wifi.h>
#include <soc/soc.h>
#include <soc/usb_serial_jtag_reg.h>

#if __has_include(<WiFi.h>)
#include <WiFi.h>
#endif

#if __has_include("esphome/core/component.h")
#include "esphome/core/component.h"
#include "esphome/core/application.h"
#endif

namespace brd_power {

inline bool epd_refresh_active{false};
inline bool entering_deep_sleep{false};

inline void arm_deep_sleep_latch() {
  entering_deep_sleep = true;
}

inline void shift_4094_config(uint8_t byte_val) {
  pinMode(0, OUTPUT);
  pinMode(12, OUTPUT);
  pinMode(13, OUTPUT);

  digitalWrite(0, LOW);
  for (int i = 7; i >= 0; --i) {
    digitalWrite(12, LOW);
    digitalWrite(13, ((byte_val >> i) & 1) ? HIGH : LOW);
    digitalWrite(12, HIGH);
  }
  digitalWrite(0, HIGH);
  digitalWrite(12, LOW);
  digitalWrite(13, LOW);
}

inline void poweroff_epd_rails_idle() {
  if (!epd_refresh_active) {
    shift_4094_config(CFG_ALL_OFF_SMPS_DISABLED);
  }
}

inline float sample_battery_rails_only(float voltage_divider = 2.0f, float max_voltage = 4.20f) {
  if (epd_refresh_active) {
    return NAN;
  }
  shift_4094_config(CFG_RAILS_ONLY_ON);
  delay(6);

  uint32_t sum_mv = 0;
  uint32_t count = 0;
  for (int attempt = 0; attempt < 16 && count < 8; ++attempt) {
    uint32_t mv = analogReadMilliVolts(14);
    if (mv > 500) {
      sum_mv += mv;
      count++;
    }
    delayMicroseconds(500);
  }

  shift_4094_config(CFG_ALL_OFF_SMPS_DISABLED);

  if (count > 0) {
    float avg_mv = static_cast<float>(sum_mv) / static_cast<float>(count);
    float voltage = (avg_mv * voltage_divider) / 1000.0f;
    return std::min(voltage, max_voltage);
  }
  return NAN;
}

inline void lock_epd_and_holds_for_deep_sleep() {
  if (!entering_deep_sleep) {
    return;
  }

  gpio_hold_dis(GPIO_NUM_0);
  gpio_hold_dis(GPIO_NUM_12);
  gpio_hold_dis(GPIO_NUM_13);

  shift_4094_config(CFG_ALL_OFF_SMPS_DISABLED);

  pinMode(12, OUTPUT);
  digitalWrite(12, LOW);
  pinMode(13, OUTPUT);
  digitalWrite(13, LOW);
  pinMode(38, OUTPUT);
  digitalWrite(38, LOW);
  pinMode(40, OUTPUT);
  digitalWrite(40, LOW);
  pinMode(41, OUTPUT);
  digitalWrite(41, LOW);
  for (int p = 1; p <= 8; ++p) {
    pinMode(p, OUTPUT);
    digitalWrite(p, LOW);
  }

  rtc_gpio_init(GPIO_NUM_0);
  rtc_gpio_set_direction(GPIO_NUM_0, RTC_GPIO_MODE_INPUT_ONLY);
  rtc_gpio_pullup_en(GPIO_NUM_0);
  rtc_gpio_pulldown_dis(GPIO_NUM_0);

  gpio_hold_en(GPIO_NUM_12);
  gpio_hold_en(GPIO_NUM_13);
  gpio_hold_en(GPIO_NUM_47);
  gpio_deep_sleep_hold_en();

#if defined(CONFIG_IDF_TARGET_ESP32S3) && defined(USB_SERIAL_JTAG_CONF0_REG)
  CLEAR_PERI_REG_MASK(USB_SERIAL_JTAG_CONF0_REG,
                      USB_SERIAL_JTAG_DP_PULLUP | USB_SERIAL_JTAG_USB_PAD_ENABLE);
#endif
}

inline void release_sleep_holds_on_boot() {
  entering_deep_sleep = false;
  rtc_gpio_deinit(GPIO_NUM_0);
  rtc_gpio_deinit(GPIO_NUM_9);
  rtc_gpio_deinit(GPIO_NUM_21);
  gpio_deep_sleep_hold_dis();
  gpio_hold_dis(GPIO_NUM_0);
  gpio_hold_dis(GPIO_NUM_9);
  gpio_hold_dis(GPIO_NUM_12);
  gpio_hold_dis(GPIO_NUM_13);
  gpio_hold_dis(GPIO_NUM_21);
  gpio_hold_dis(GPIO_NUM_47);
}

inline void register_power_latch() {
  // Empty inline hook for component lifecycle registration.
}

inline bool check_usb_sof_incremented() {
#if defined(CONFIG_IDF_TARGET_ESP32S3) && defined(USB_SERIAL_JTAG_FRAM_NUM_REG)
  const uint32_t frame1 = REG_READ(USB_SERIAL_JTAG_FRAM_NUM_REG) & USB_SERIAL_JTAG_SOF_FRAME_INDEX;
  delay(3);
  const uint32_t frame2 = REG_READ(USB_SERIAL_JTAG_FRAM_NUM_REG) & USB_SERIAL_JTAG_SOF_FRAME_INDEX;
  return frame1 != frame2;
#else
  return false;
#endif
}

inline int sleep_gt911_and_hold_int_low(brd_rtc::I2cPort &i2c, int preferred_addr = 0) {
  // Drive interrupt line low before sending sleep command.
  pinMode(47, OUTPUT);
  digitalWrite(47, LOW);
  delay(5);

  const uint8_t sleep_cmd[3] = {0x80, 0x40, 0x05};
  int ack_addr = 0;

  const uint8_t primary_addr = (preferred_addr == 2) ? 0x14 : 0x5D;
  const uint8_t secondary_addr = (preferred_addr == 2) ? 0x5D : 0x14;
  const int primary_code = (preferred_addr == 2) ? 2 : 1;
  const int secondary_code = (preferred_addr == 2) ? 1 : 2;

  if (i2c.write(primary_addr, sleep_cmd, 3)) {
    ack_addr = primary_code;
  } else if (i2c.write(secondary_addr, sleep_cmd, 3)) {
    ack_addr = secondary_code;
  } else {
    // Pulse interrupt high to wake controller and retry.
    digitalWrite(47, HIGH);
    delay(10);
    digitalWrite(47, LOW);
    delay(5);

    if (i2c.write(primary_addr, sleep_cmd, 3)) {
      ack_addr = primary_code;
    } else if (i2c.write(secondary_addr, sleep_cmd, 3)) {
      ack_addr = secondary_code;
    }
  }

  delay(25);

  // Hold interrupt line low during deep sleep.
  pinMode(47, OUTPUT);
  digitalWrite(47, LOW);
  gpio_hold_en(GPIO_NUM_47);

  return ack_addr;
}

inline void prepare_esp32s3_deep_sleep_peripherals() {
  gpio_deep_sleep_hold_en();

  rtc_gpio_init(GPIO_NUM_9);
  rtc_gpio_set_direction(GPIO_NUM_9, RTC_GPIO_MODE_INPUT_ONLY);
  rtc_gpio_pullup_en(GPIO_NUM_9);
  rtc_gpio_pulldown_dis(GPIO_NUM_9);

  rtc_gpio_init(GPIO_NUM_21);
  rtc_gpio_set_direction(GPIO_NUM_21, RTC_GPIO_MODE_INPUT_ONLY);
  rtc_gpio_pullup_en(GPIO_NUM_21);
  rtc_gpio_pulldown_dis(GPIO_NUM_21);

#if __has_include(<WiFi.h>)
  WiFi.disconnect(true);
#endif
  esp_wifi_stop();
}

}  // namespace brd_power

#endif  // USE_ESP32
