#pragma once

#include <cstddef>
#include <cstdint>

namespace brd_rtc {

constexpr uint8_t DEFAULT_RTC_ADDR = 0x51;

// Convert decimal value to Binary Coded Decimal.
constexpr uint8_t dec_to_bcd(uint8_t val) {
  return static_cast<uint8_t>(((val / 10) << 4) | (val % 10));
}

// Convert Binary Coded Decimal value to decimal.
constexpr uint8_t bcd_to_dec(uint8_t val) {
  return static_cast<uint8_t>(((val >> 4) * 10) + (val & 0x0F));
}

// Verify that both nibbles contain valid decimal digits.
constexpr bool is_valid_bcd(uint8_t val) {
  return ((val & 0x0F) <= 9) && ((val >> 4) <= 9);
}

// Compute ESPTime weekday index (1 for Sunday through 7 for Saturday).
constexpr uint8_t compute_esphome_weekday(uint16_t year, uint8_t month, uint8_t day) {
  if (year < 1 || month < 1 || month > 12 || day < 1 || day > 31) {
    return 0;
  }
  constexpr int MONTH_OFFSETS[12] = {0, 3, 2, 5, 0, 3, 5, 1, 4, 6, 2, 4};
  int y = static_cast<int>(year);
  if (month < 3) {
    --y;
  }
  const int dow0 =
      (y + y / 4 - y / 100 + y / 400 + MONTH_OFFSETS[month - 1] + static_cast<int>(day)) % 7;
  return static_cast<uint8_t>(dow0 + 1);
}

class I2cPort {
 public:
  virtual ~I2cPort() = default;
  virtual bool write(uint8_t address, const uint8_t *data, size_t len) = 0;
  virtual bool write_read(uint8_t address, const uint8_t *write_data, size_t write_len,
                          uint8_t *read_data, size_t read_len) = 0;
};

struct RtcStatus {
  bool alarm_flag{false};
  bool timer_flag{false};
};

struct UtcClock {
  uint16_t year{2026};
  uint8_t month{1};
  uint8_t day{1};
  uint8_t weekday{0};
  uint8_t hour{0};
  uint8_t minute{0};
  uint8_t second{0};
};

struct WakeSettings {
  bool use_alarm{false};
  uint8_t target_utc_hour{0};
  uint8_t target_utc_minute{0};
  uint32_t sleep_seconds{0};
};

inline bool retry_write(I2cPort &i2c, uint8_t addr, const uint8_t *data, size_t len) {
  for (int attempt = 0; attempt < 3; ++attempt) {
    if (i2c.write(addr, data, len)) {
      return true;
    }
  }
  return false;
}

inline bool retry_write_read(I2cPort &i2c, uint8_t addr, const uint8_t *wdata, size_t wlen,
                             uint8_t *rdata, size_t rlen) {
  for (int attempt = 0; attempt < 3; ++attempt) {
    if (i2c.write_read(addr, wdata, wlen, rdata, rlen)) {
      return true;
    }
  }
  return false;
}

// Repair weekday register when voltage-low flag is clear and date is valid.
inline bool repair_sunday_weekday_if_valid(I2cPort &i2c, uint8_t rtc_addr) {
  const uint8_t reg02_addr = 0x02;
  uint8_t buf[7]{};
  if (!retry_write_read(i2c, rtc_addr, &reg02_addr, 1, buf, sizeof(buf))) {
    return false;
  }
  if ((buf[0] & 0x80) != 0) {
    return true;
  }

  const uint8_t day_bcd = buf[3] & 0x3F;
  const uint8_t month_bcd = buf[5] & 0x1F;
  const uint8_t year_bcd = buf[6];
  if (!is_valid_bcd(day_bcd) || !is_valid_bcd(month_bcd) || !is_valid_bcd(year_bcd)) {
    return true;
  }

  const uint8_t day = bcd_to_dec(day_bcd);
  const uint8_t month = bcd_to_dec(month_bcd);
  const uint16_t year = static_cast<uint16_t>(2000U + bcd_to_dec(year_bcd));
  if (year < 2024 || month < 1 || month > 12 || day < 1 || day > 31) {
    return true;
  }

  const uint8_t expected_wday = compute_esphome_weekday(year, month, day);
  if (expected_wday < 1 || expected_wday > 7) {
    return true;
  }

  if ((buf[4] & 0x07) != expected_wday) {
    const uint8_t repair_cmd[2] = {0x06, static_cast<uint8_t>(dec_to_bcd(expected_wday) & 0x07)};
    return retry_write(i2c, rtc_addr, repair_cmd, 2);
  }
  return true;
}

// Read status flags before clearing register 0x01 and disable clock output.
inline bool read_and_clear_status_and_disable_clkout(I2cPort &i2c, uint8_t rtc_addr,
                                                    RtcStatus &status) {
  const uint8_t reg01_addr = 0x01;
  uint8_t reg01_val = 0;
  if (!retry_write_read(i2c, rtc_addr, &reg01_addr, 1, &reg01_val, 1)) {
    return false;
  }

  status.alarm_flag = (reg01_val & 0x08) != 0;
  status.timer_flag = (reg01_val & 0x04) != 0;

  const uint8_t clear_status[2] = {0x01, 0x00};
  if (!retry_write(i2c, rtc_addr, clear_status, 2)) {
    return false;
  }

  const uint8_t disable_clkout[2] = {0x0D, 0x00};
  if (!retry_write(i2c, rtc_addr, disable_clkout, 2)) {
    return false;
  }

  return repair_sunday_weekday_if_valid(i2c, rtc_addr);
}

// Write UTC clock values to registers 0x02 through 0x08, ensure the oscillator runs, and disable clock output.
inline bool write_utc_clock_only(I2cPort &i2c, uint8_t rtc_addr, const UtcClock &clk) {
  if (clk.year < 2000 || clk.year > 2099 || clk.hour > 23 || clk.minute > 59 || clk.second > 59) {
    return false;
  }
  const uint8_t esphome_wday = compute_esphome_weekday(clk.year, clk.month, clk.day);
  if (esphome_wday < 1 || esphome_wday > 7) {
    return false;
  }

  uint8_t buf[8];
  buf[0] = 0x02;
  buf[1] = dec_to_bcd(clk.second) & 0x7F;
  buf[2] = dec_to_bcd(clk.minute) & 0x7F;
  buf[3] = dec_to_bcd(clk.hour) & 0x3F;
  buf[4] = dec_to_bcd(clk.day) & 0x3F;
  buf[5] = dec_to_bcd(esphome_wday) & 0x07;
  buf[6] = dec_to_bcd(clk.month) & 0x1F;
  buf[7] = dec_to_bcd(static_cast<uint8_t>(clk.year % 100));

  if (!retry_write(i2c, rtc_addr, buf, 8)) {
    return false;
  }

  // Clear Control_status_1 (STOP bit) so the hardware oscillator runs.
  const uint8_t run_osc_buf[2] = {0x00, 0x00};
  if (!retry_write(i2c, rtc_addr, run_osc_buf, 2)) {
    return false;
  }

  const uint8_t clkout_buf[2] = {0x0D, 0x00};
  return retry_write(i2c, rtc_addr, clkout_buf, 2);
}

// Configure wake alarm or timer on hardware RTC with verification.
inline bool arm_wake(I2cPort &i2c, uint8_t rtc_addr, const WakeSettings &settings) {
  // Stop countdown timer first.
  const uint8_t stop_timer[2] = {0x0E, 0x00};
  if (!retry_write(i2c, rtc_addr, stop_timer, 2)) {
    return false;
  }

  // Clear control and status flags.
  const uint8_t clear_ctrl[2] = {0x01, 0x00};
  if (!retry_write(i2c, rtc_addr, clear_ctrl, 2)) {
    return false;
  }

  if (settings.use_alarm) {
    const uint8_t min_bcd = dec_to_bcd(settings.target_utc_minute) & 0x7F;
    const uint8_t hr_bcd = dec_to_bcd(settings.target_utc_hour) & 0x3F;
    const uint8_t alarm_cfg[5] = {0x09, min_bcd, hr_bcd, 0x80, 0x80};

    if (!retry_write(i2c, rtc_addr, alarm_cfg, 5)) {
      return false;
    }

    // Enable alarm interrupt flag.
    const uint8_t enable_aie[2] = {0x01, 0x02};
    if (!retry_write(i2c, rtc_addr, enable_aie, 2)) {
      return false;
    }

    // Read register 0x01 to verify the alarm interrupt flag.
    const uint8_t reg01_addr = 0x01;
    uint8_t reg01_val = 0;
    if (!retry_write_read(i2c, rtc_addr, &reg01_addr, 1, &reg01_val, 1)) {
      return false;
    }
    if ((reg01_val & 0x02) == 0) {
      return false;
    }

    // Read alarm registers to verify the target time.
    const uint8_t reg09_addr = 0x09;
    uint8_t alarm_rb[2] = {0, 0};
    if (!retry_write_read(i2c, rtc_addr, &reg09_addr, 1, alarm_rb, 2)) {
      return false;
    }
    if ((alarm_rb[0] & 0x7F) != min_bcd || (alarm_rb[1] & 0x3F) != hr_bcd) {
      return false;
    }

    return true;
  }

  // Disable alarm registers when timer operates.
  const uint8_t disable_alarm[3] = {0x09, 0x80, 0x80};
  if (!retry_write(i2c, rtc_addr, disable_alarm, 3)) {
    return false;
  }

  uint8_t timer_ctrl = 0;
  uint8_t timer_val = 0;
  if (settings.sleep_seconds > 255) {
    timer_ctrl = 0x83;
    uint32_t mins = (settings.sleep_seconds + 30U) / 60U;
    if (mins > 255) {
      mins = 255;
    }
    if (mins == 0) {
      mins = 1;
    }
    timer_val = static_cast<uint8_t>(mins);
  } else {
    timer_ctrl = 0x82;
    uint32_t secs = settings.sleep_seconds;
    if (secs > 255) {
      secs = 255;
    }
    if (secs == 0) {
      secs = 1;
    }
    timer_val = static_cast<uint8_t>(secs);
  }

  const uint8_t timer_cfg[3] = {0x0E, timer_ctrl, timer_val};
  if (!retry_write(i2c, rtc_addr, timer_cfg, 3)) {
    return false;
  }

  // Enable timer interrupt.
  const uint8_t enable_tie[2] = {0x01, 0x01};
  if (!retry_write(i2c, rtc_addr, enable_tie, 2)) {
    return false;
  }

  // Read register 0x01 to verify the timer interrupt flag.
  const uint8_t reg01_addr = 0x01;
  uint8_t reg01_val = 0;
  if (!retry_write_read(i2c, rtc_addr, &reg01_addr, 1, &reg01_val, 1)) {
    return false;
  }
  if ((reg01_val & 0x01) == 0) {
    return false;
  }

  // Read register 0x0E to verify timer activation.
  const uint8_t reg0e_addr = 0x0E;
  uint8_t reg0e_val = 0;
  if (!retry_write_read(i2c, rtc_addr, &reg0e_addr, 1, &reg0e_val, 1)) {
    return false;
  }
  if ((reg0e_val & 0x80) == 0) {
    return false;
  }

  return true;
}

#if __has_include("esphome/components/i2c/i2c_bus.h")
#include "esphome/components/i2c/i2c_bus.h"

class EspHomeI2cAdapter : public I2cPort {
 public:
  explicit EspHomeI2cAdapter(esphome::i2c::I2CBus *bus) : bus_(bus) {}
  explicit EspHomeI2cAdapter(esphome::i2c::I2CBus &bus) : bus_(&bus) {}

  bool write(uint8_t address, const uint8_t *data, size_t len) override {
    if (this->bus_ == nullptr) {
      return false;
    }
    return this->bus_->write_readv(address, data, len, nullptr, 0) == esphome::i2c::ERROR_OK;
  }

  bool write_read(uint8_t address, const uint8_t *write_data, size_t write_len,
                  uint8_t *read_data, size_t read_len) override {
    if (this->bus_ == nullptr) {
      return false;
    }
    return this->bus_->write_readv(address, write_data, write_len, read_data, read_len) ==
           esphome::i2c::ERROR_OK;
  }

 private:
  esphome::i2c::I2CBus *bus_{nullptr};
};
#endif

}  // namespace brd_rtc
