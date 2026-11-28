#include <cassert>
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <limits>
#include <thread>
#include <vector>

#include "../brd_blit.h"
#include "../brd_schedule.h"
#include "../brd_rtc.h"
#include "../brd_power.h"

// Simulate an I2C bus to check retries, write order, and register state.
class MockI2cBus : public brd_rtc::I2cPort {
 public:
  struct Operation {
    uint8_t address;
    std::vector<uint8_t> write_data;
    bool is_read;
  };

  std::vector<Operation> history;
  uint8_t registers[256]{};
  bool override_read[256]{};
  uint8_t override_val[256]{};
  int fail_count{0};
  int fail_read_count{0};
  bool inject_readback_mismatch{false};
  bool decrement_timer_on_start{false};

  MockI2cBus() {
    std::memset(this->registers, 0, sizeof(this->registers));
    std::memset(this->override_read, 0, sizeof(this->override_read));
    std::memset(this->override_val, 0, sizeof(this->override_val));
  }

  void clear_read_overrides() {
    std::memset(this->override_read, 0, sizeof(this->override_read));
    std::memset(this->override_val, 0, sizeof(this->override_val));
  }

  bool write(uint8_t address, const uint8_t *data, size_t len) override {
    if (data == nullptr && len > 0) {
      return false;
    }
    if (this->fail_count > 0) {
      this->fail_count--;
      return false;
    }
    if (len >= 1) {
      const size_t reg = static_cast<size_t>(data[0]);
      if (reg + (len - 1) > sizeof(this->registers)) {
        return false;
      }
      this->history.push_back({address, std::vector<uint8_t>(data, data + len), false});
      for (size_t i = 1; i < len; ++i) {
        const size_t target_reg = reg + (i - 1);
        this->registers[target_reg] = data[i];
        if (this->decrement_timer_on_start && target_reg == 0x0E && (data[i] & 0x80U) != 0 &&
            this->registers[0x0F] > 0U) {
          this->registers[0x0F]--;
        }
      }
    } else {
      this->history.push_back({address, {}, false});
    }
    return true;
  }

  bool write_read(uint8_t address, const uint8_t *write_data, size_t write_len,
                  uint8_t *read_data, size_t read_len) override {
    if ((write_data == nullptr && write_len > 0) || (read_data == nullptr && read_len > 0)) {
      return false;
    }
    if (this->fail_count > 0) {
      this->fail_count--;
      return false;
    }
    if (this->fail_read_count > 0) {
      this->fail_read_count--;
      return false;
    }
    if (write_len >= 1 && read_len > 0) {
      const size_t reg = static_cast<size_t>(write_data[0]);
      if (reg + read_len > sizeof(this->registers)) {
        return false;
      }
      this->history.push_back(
          {address, std::vector<uint8_t>(write_data, write_data + write_len), true});
      for (size_t i = 0; i < read_len; ++i) {
        if (this->inject_readback_mismatch) {
          read_data[i] = 0xFF;
        } else if (this->override_read[reg + i]) {
          read_data[i] = this->override_val[reg + i];
        } else {
          read_data[i] = this->registers[reg + i];
        }
      }
    } else {
      this->history.push_back(
          {address,
           write_data != nullptr ? std::vector<uint8_t>(write_data, write_data + write_len)
                                 : std::vector<uint8_t>{},
           true});
    }
    return true;
  }
};

static void test_blit_bounds_and_errors() {
  printf("[TEST] brd_blit framebuffer copy and bounds checks...\n");

  std::vector<uint8_t> dst(brd_blit::FRAMEBUFFER_SIZE);
  std::vector<uint8_t> src(brd_blit::FRAMEBUFFER_SIZE, 0xAB);

  // Exact size copy succeeds.
  auto res = brd_blit::copy_framebuffer(dst.data(), dst.size(), src.data(), src.size());
  assert(res == brd_blit::BlitResult::OK);
  assert(dst[0] == 0xAB && dst[brd_blit::FRAMEBUFFER_SIZE - 1] == 0xAB);

  // Overlapping self-copy succeeds safely via memmove.
  res = brd_blit::copy_framebuffer(dst.data(), dst.size(), dst.data(), dst.size());
  assert(res == brd_blit::BlitResult::OK);
  assert(dst[0] == 0xAB && dst[brd_blit::FRAMEBUFFER_SIZE - 1] == 0xAB);

  // Null pointer checks return errors.
  assert(brd_blit::copy_framebuffer(nullptr, dst.size(), src.data(), src.size()) ==
         brd_blit::BlitResult::ERROR_NULL_POINTER);
  assert(brd_blit::copy_framebuffer(dst.data(), dst.size(), nullptr, src.size()) ==
         brd_blit::BlitResult::ERROR_NULL_POINTER);

  // Truncated source returns error.
  assert(brd_blit::copy_framebuffer(dst.data(), dst.size(), src.data(), src.size() - 1) ==
         brd_blit::BlitResult::ERROR_SIZE_MISMATCH);

  // Oversized source returns error.
  assert(brd_blit::copy_framebuffer(dst.data(), dst.size(), src.data(), src.size() + 1) ==
         brd_blit::BlitResult::ERROR_SIZE_MISMATCH);

  // Target overflow returns error.
  assert(brd_blit::copy_framebuffer(dst.data(), dst.size() - 1, src.data(), src.size()) ==
         brd_blit::BlitResult::ERROR_TARGET_OVERFLOW);

  // Content-Length pre-check allows unknown (0 or SIZE_MAX) and exact match, rejects mismatches.
  assert(brd_blit::is_expected_content_length(0U, brd_blit::FRAMEBUFFER_SIZE));
  assert(brd_blit::is_expected_content_length(static_cast<size_t>(-1), brd_blit::FRAMEBUFFER_SIZE));
  assert(brd_blit::is_expected_content_length(brd_blit::FRAMEBUFFER_SIZE, brd_blit::FRAMEBUFFER_SIZE));
  assert(!brd_blit::is_expected_content_length(1U, brd_blit::FRAMEBUFFER_SIZE));
  assert(!brd_blit::is_expected_content_length(brd_blit::FRAMEBUFFER_SIZE - 1, brd_blit::FRAMEBUFFER_SIZE));
  assert(!brd_blit::is_expected_content_length(brd_blit::FRAMEBUFFER_SIZE + 1, brd_blit::FRAMEBUFFER_SIZE));

  // StagingBuffer RAII allocation and copy into framebuffer.
  brd_blit::StagingBuffer empty_stage(0U);
  assert(!empty_stage.valid() && empty_stage.data() == nullptr && empty_stage.size() == 0U);
  brd_blit::StagingBuffer stage(brd_blit::FRAMEBUFFER_SIZE);
  assert(stage.valid() && stage.data() != nullptr && stage.size() == brd_blit::FRAMEBUFFER_SIZE);
  std::memset(stage.data(), 0x5A, stage.size());
  assert(brd_blit::copy_framebuffer(dst.data(), dst.size(), stage.data(), stage.size()) ==
         brd_blit::BlitResult::OK);
  assert(dst[0] == 0x5A && dst[brd_blit::FRAMEBUFFER_SIZE - 1] == 0x5A);

  printf("  -> Passed blit bounds checks.\n");
}

static void test_blit_sha256_normalizer() {
  printf("[TEST] brd_blit SHA-256 parser and normalizer...\n");

  char out[65];

  // Standard valid lowercase hex.
  const char *valid_lower = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855";
  assert(brd_blit::parse_and_normalize_sha256(valid_lower, strlen(valid_lower), out));
  assert(std::strcmp(out, valid_lower) == 0);

  // Mixed case with whitespace and newline.
  const char *mixed_ws = "  E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855 \r\n";
  assert(brd_blit::parse_and_normalize_sha256(mixed_ws, strlen(mixed_ws), out));
  assert(std::strcmp(out, valid_lower) == 0);

  // Null and empty checks fail and NUL-terminate output.
  std::memset(out, 'X', sizeof(out));
  assert(!brd_blit::parse_and_normalize_sha256(nullptr, 64, out));
  assert(out[0] == '\0');
  std::memset(out, 'X', sizeof(out));
  assert(!brd_blit::parse_and_normalize_sha256(valid_lower, 0, out));
  assert(out[0] == '\0');
  assert(!brd_blit::parse_and_normalize_sha256(valid_lower, 64, nullptr));

  // Too short input fails and NUL-terminates output.
  const char *short_sha = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85";
  std::memset(out, 'X', sizeof(out));
  assert(!brd_blit::parse_and_normalize_sha256(short_sha, strlen(short_sha), out));
  assert(out[0] == '\0');

  // Too long input fails and NUL-terminates output.
  const char *long_sha = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855aa";
  std::memset(out, 'X', sizeof(out));
  assert(!brd_blit::parse_and_normalize_sha256(long_sha, strlen(long_sha), out));
  assert(out[0] == '\0');

  // Invalid non-hex characters fail and NUL-terminate output.
  const char *invalid_hex = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85g";
  std::memset(out, 'X', sizeof(out));
  assert(!brd_blit::parse_and_normalize_sha256(invalid_hex, strlen(invalid_hex), out));
  assert(out[0] == '\0');

  printf("  -> Passed SHA-256 normalizer checks.\n");
}

static void test_blit_thread_safe_buffer() {
  printf("[TEST] brd_blit BlitBuffer thread safety...\n");

  brd_blit::BlitBuffer buffer;
  assert(!buffer.is_valid());

  std::vector<uint8_t> data(brd_blit::FRAMEBUFFER_SIZE, 0x42);

  // Reject null pointers and mismatched sizes.
  assert(!buffer.store(nullptr, data.size()));
  assert(!buffer.store(data.data(), data.size() - 1));
  assert(!buffer.store(data.data(), data.size() + 1));
  assert(!buffer.is_valid());

  assert(buffer.store(data.data(), data.size()));
  assert(buffer.is_valid());

  std::vector<uint8_t> read_back(brd_blit::FRAMEBUFFER_SIZE, 0x00);
  assert(!buffer.retrieve(nullptr, read_back.size()));
  assert(!buffer.retrieve(read_back.data(), read_back.size() - 1));
  assert(buffer.retrieve(read_back.data(), read_back.size()));
  assert(read_back[0] == 0x42 && read_back[brd_blit::FRAMEBUFFER_SIZE - 1] == 0x42);

  buffer.invalidate();
  assert(!buffer.is_valid());
  assert(!buffer.retrieve(read_back.data(), read_back.size()));

  printf("  -> Passed BlitBuffer basic operations.\n");
}

static void test_schedule_civil_normalization() {
  printf("[TEST] brd_schedule days_in_month and normalize_civil_datetime...\n");

  assert(brd_schedule::days_in_month(2025, 0) == 0);
  assert(brd_schedule::days_in_month(2025, 13) == 0);
  assert(brd_schedule::days_in_month(2024, 2) == 29);
  assert(brd_schedule::days_in_month(2025, 2) == 28);
  assert(brd_schedule::days_in_month(2000, 2) == 29);
  assert(brd_schedule::days_in_month(2100, 2) == 28);
  assert(brd_schedule::days_in_month(2026, 3) == 31);
  assert(brd_schedule::days_in_month(2026, 4) == 30);

  struct NormCase {
    brd_schedule::LocalTime in;
    brd_schedule::LocalTime out;
  };
  const NormCase cases[] = {
      // Non-leap Feb 28 -> Mar 1 on hour overflow.
      {{2025, 2, 28, 24, 0, 0}, {2025, 3, 1, 0, 0, 0}},
      // Leap Feb 28 -> Feb 29, and Feb 29 -> Mar 1.
      {{2024, 2, 28, 24, 0, 0}, {2024, 2, 29, 0, 0, 0}},
      {{2024, 2, 29, 25, 0, 0}, {2024, 3, 1, 1, 0, 0}},
      // Century non-leap and 400-year leap boundaries.
      {{2100, 2, 28, 24, 0, 0}, {2100, 3, 1, 0, 0, 0}},
      {{2000, 2, 28, 24, 0, 0}, {2000, 2, 29, 0, 0, 0}},
      // Mar 31 -> Apr 1 and Dec 31 -> Jan 1 carry.
      {{2026, 3, 31, 24, 0, 0}, {2026, 4, 1, 0, 0, 0}},
      {{2026, 12, 31, 23, 60, 0}, {2027, 1, 1, 0, 0, 0}},
      {{2026, 12, 31, 23, 59, 60}, {2027, 1, 1, 0, 0, 0}},
      // Multi-unit overflow across seconds, minutes, hours, and months.
      {{2026, 1, 31, 50, 125, -120}, {2026, 2, 2, 4, 3, 0}},
      {{2026, 13, 1, 0, 0, 0}, {2027, 1, 1, 0, 0, 0}},
      // Borrow across midnight, months, and leap years.
      {{2026, 1, 1, 0, -1, 0}, {2025, 12, 31, 23, 59, 0}},
      {{2026, 1, 1, -25, 0, 0}, {2025, 12, 30, 23, 0, 0}},
      {{2024, 3, 1, 0, 0, -1}, {2024, 2, 29, 23, 59, 59}},
      {{2025, 3, 1, 0, 0, -1}, {2025, 2, 28, 23, 59, 59}},
      {{2026, 0, 15, 0, 0, 0}, {2025, 12, 15, 0, 0, 0}},
      {{2026, 3, 0, 12, 0, 0}, {2026, 2, 28, 12, 0, 0}},
  };

  for (const auto &c : cases) {
    brd_schedule::LocalTime actual = c.in;
    assert(brd_schedule::normalize_civil_datetime(actual));
    assert(actual.year == c.out.year);
    assert(actual.month == c.out.month);
    assert(actual.day == c.out.day);
    assert(actual.hour == c.out.hour);
    assert(actual.minute == c.out.minute);
    assert(actual.second == c.out.second);
  }

  brd_schedule::LocalTime bad{0, 1, 1, 0, 0, 0};
  assert(!brd_schedule::normalize_civil_datetime(bad));

  printf("  -> Passed civil datetime normalization checks.\n");
}

static void test_schedule_dst_and_boundaries() {
  printf("[TEST] brd_schedule POSIX CET/CEST and UTC transitions...\n");

  brd_schedule::PosixTzCalendar cal("CET-1CEST,M3.5.0,M10.5.0/3");
  brd_schedule::PosixTzCalendar cal_utc;
  brd_schedule::ScheduleConfig cfg;
  cfg.wake_hour = 6;
  cfg.wake_minute = 20;
  cfg.sleep_hour = 21;
  cfg.sleep_minute = 45;

  // Invalid clock returns fallback plan.
  auto plan_zero = brd_schedule::plan_next_wake(cal, 0, cfg);
  assert(!plan_zero.clock_valid);
  assert(std::strcmp(plan_zero.mode, "timer") == 0);
  assert(plan_zero.sleep_seconds == 900);

  // Pre-2020 epoch returns fallback plan.
  auto plan_old = brd_schedule::plan_next_wake(cal, 1000000, cfg);
  assert(!plan_old.clock_valid);

  // Test default PosixTzCalendar uses UTC.
  int64_t utc_noon = 0;
  assert(cal_utc.local_to_epoch({2026, 7, 15, 12, 0, 0}, utc_noon));
  brd_schedule::WallTime utc_check;
  assert(cal_utc.epoch_to_utc(utc_noon, utc_check));
  assert(utc_check.hour == 12 && utc_check.minute == 0 && utc_check.second == 0);

  // Test Spring-Forward DST Transition: March 29, 2026.
  brd_schedule::WallTime t_spring;
  t_spring.year = 2026;
  t_spring.month = 3;
  t_spring.day = 29;
  t_spring.hour = 10;
  t_spring.minute = 13;
  t_spring.second = 5;
  int64_t epoch_spring = 0;
  assert(cal.local_to_epoch(t_spring, epoch_spring));

  auto plan_spring = brd_schedule::plan_next_wake(cal, epoch_spring, cfg);
  assert(plan_spring.clock_valid);
  assert(std::strcmp(plan_spring.mode, "timer") == 0);
  assert(plan_spring.arm_alarm);

  brd_schedule::WallTime local_target;
  assert(cal.epoch_to_local(plan_spring.target_epoch, local_target));
  assert(local_target.hour == 10 && local_target.minute == 30 && local_target.second == 0);

  // Test Fall-Back DST Transition: October 25, 2026.
  brd_schedule::WallTime t_fall;
  t_fall.year = 2026;
  t_fall.month = 10;
  t_fall.day = 25;
  t_fall.hour = 14;
  t_fall.minute = 0;
  t_fall.second = 0;
  int64_t epoch_fall = 0;
  assert(cal.local_to_epoch(t_fall, epoch_fall));

  auto plan_fall = brd_schedule::plan_next_wake(cal, epoch_fall, cfg);
  assert(plan_fall.clock_valid);
  assert(cal.epoch_to_local(plan_fall.target_epoch, local_target));
  assert(local_target.hour == 14 && local_target.minute == 15 && local_target.second == 0);

  // Test 5-minute boundary interval: 14:00:10 -> 14:05:00, and 14:03:10 skips to 14:10:00 due to 120s lead margin.
  brd_schedule::ScheduleConfig cfg_5m = cfg;
  cfg_5m.boundary_interval_min = 5;
  cfg_5m.fallback_sleep_sec = 300;
  int64_t epoch_5m_a = 0;
  assert(cal.local_to_epoch({2026, 7, 15, 14, 0, 10}, epoch_5m_a));
  auto plan_5m_a = brd_schedule::plan_next_wake(cal, epoch_5m_a, cfg_5m);
  assert(plan_5m_a.clock_valid);
  assert(plan_5m_a.arm_alarm);
  assert(cal.epoch_to_local(plan_5m_a.target_epoch, local_target));
  assert(local_target.hour == 14 && local_target.minute == 5 && local_target.second == 0);

  int64_t epoch_5m_b = 0;
  assert(cal.local_to_epoch({2026, 7, 15, 14, 3, 10}, epoch_5m_b));
  auto plan_5m_b = brd_schedule::plan_next_wake(cal, epoch_5m_b, cfg_5m);
  assert(plan_5m_b.clock_valid);
  assert(plan_5m_b.arm_alarm);
  assert(cal.epoch_to_local(plan_5m_b.target_epoch, local_target));
  assert(local_target.hour == 14 && local_target.minute == 10 && local_target.second == 0);

  // Non-positive boundary_interval_min falls back to 15-minute boundaries instead of dividing by zero.
  brd_schedule::ScheduleConfig cfg_zero_int = cfg;
  cfg_zero_int.boundary_interval_min = 0;
  auto plan_zero_int = brd_schedule::plan_next_wake(cal, epoch_fall, cfg_zero_int);
  assert(plan_zero_int.clock_valid);
  assert(cal.epoch_to_local(plan_zero_int.target_epoch, local_target));
  assert(local_target.hour == 14 && local_target.minute == 15 && local_target.second == 0);

  // Test Bedtime Clamping: 21:43:00 has mode "timer".
  brd_schedule::WallTime t_bed_clamp;
  t_bed_clamp.year = 2026;
  t_bed_clamp.month = 7;
  t_bed_clamp.day = 15;
  t_bed_clamp.hour = 21;
  t_bed_clamp.minute = 43;
  t_bed_clamp.second = 0;
  int64_t epoch_bed_clamp = 0;
  assert(cal.local_to_epoch(t_bed_clamp, epoch_bed_clamp));

  auto plan_bed_clamp = brd_schedule::plan_next_wake(cal, epoch_bed_clamp, cfg);
  assert(plan_bed_clamp.clock_valid);
  assert(std::strcmp(plan_bed_clamp.mode, "timer") == 0);
  assert(cal.epoch_to_local(plan_bed_clamp.target_epoch, local_target));
  assert(local_target.hour == 21 && local_target.minute == 45 && local_target.second == 0);

  // Test Bedtime Advance: 21:44:10 advances to morning 06:20 with mode "alarm".
  brd_schedule::WallTime t_bed_adv;
  t_bed_adv.year = 2026;
  t_bed_adv.month = 7;
  t_bed_adv.day = 15;
  t_bed_adv.hour = 21;
  t_bed_adv.minute = 44;
  t_bed_adv.second = 10;
  int64_t epoch_bed_adv = 0;
  assert(cal.local_to_epoch(t_bed_adv, epoch_bed_adv));

  auto plan_bed_adv = brd_schedule::plan_next_wake(cal, epoch_bed_adv, cfg);
  assert(plan_bed_adv.clock_valid);
  assert(std::strcmp(plan_bed_adv.mode, "alarm") == 0);
  assert(cal.epoch_to_local(plan_bed_adv.target_epoch, local_target));
  assert(local_target.hour == 6 && local_target.minute == 20 && local_target.second == 0);
  assert(local_target.day == 16);

  // Test Overnight Sleep: 23:00:00 wakes at 06:20 next morning with mode "alarm".
  brd_schedule::WallTime t_night;
  t_night.year = 2026;
  t_night.month = 7;
  t_night.day = 15;
  t_night.hour = 23;
  t_night.minute = 0;
  t_night.second = 0;
  int64_t epoch_night = 0;
  assert(cal.local_to_epoch(t_night, epoch_night));

  auto plan_night = brd_schedule::plan_next_wake(cal, epoch_night, cfg);
  assert(plan_night.clock_valid);
  assert(std::strcmp(plan_night.mode, "alarm") == 0);
  assert(cal.epoch_to_local(plan_night.target_epoch, local_target));
  assert(local_target.hour == 6 && local_target.minute == 20 && local_target.second == 0);
  assert(local_target.day == 16);

  // Test Early Morning before Wake: 04:30:00 wakes at 06:20 today with mode "alarm".
  brd_schedule::WallTime t_early;
  t_early.year = 2026;
  t_early.month = 7;
  t_early.day = 16;
  t_early.hour = 4;
  t_early.minute = 30;
  t_early.second = 0;
  int64_t epoch_early = 0;
  assert(cal.local_to_epoch(t_early, epoch_early));

  auto plan_early = brd_schedule::plan_next_wake(cal, epoch_early, cfg);
  assert(plan_early.clock_valid);
  assert(std::strcmp(plan_early.mode, "alarm") == 0);
  assert(cal.epoch_to_local(plan_early.target_epoch, local_target));
  assert(local_target.hour == 6 && local_target.minute == 20 && local_target.second == 0);
  assert(local_target.day == 16);

  // Test Revalidation: remaining time >= 90s updates sleep_seconds and recalculates UTC target.
  brd_schedule::SchedulePlan input_plan = plan_spring;
  input_plan.target_utc_hour = 0;
  input_plan.target_utc_minute = 0;
  auto plan_reval_ok = brd_schedule::revalidate_before_sleep(cal, epoch_spring + 30, input_plan, cfg);
  assert(plan_reval_ok.target_epoch == plan_spring.target_epoch);
  assert(plan_reval_ok.sleep_seconds == plan_spring.sleep_seconds - 30);
  assert(plan_reval_ok.arm_alarm);
  assert(plan_reval_ok.target_utc_hour == plan_spring.target_utc_hour);
  assert(plan_reval_ok.target_utc_minute == plan_spring.target_utc_minute);

  // Test Revalidation: upgrades plan when target_epoch == 0.
  brd_schedule::SchedulePlan zero_plan;
  zero_plan.clock_valid = false;
  zero_plan.target_epoch = 0;
  auto plan_reval_upgraded = brd_schedule::revalidate_before_sleep(cal, epoch_spring, zero_plan, cfg);
  assert(plan_reval_upgraded.clock_valid);
  assert(plan_reval_upgraded.target_epoch == plan_spring.target_epoch);

  // Check Revalidation: remaining time < 90s recomputes schedule.
  auto plan_reval_stale = brd_schedule::revalidate_before_sleep(cal, plan_spring.target_epoch - 30, plan_spring, cfg);
  assert(plan_reval_stale.target_epoch != plan_spring.target_epoch);

  // Check Revalidation: now_epoch <= 0 returns fallback with sleep_seconds >= 60.
  brd_schedule::SchedulePlan tiny_plan = plan_spring;
  tiny_plan.sleep_seconds = 0;
  brd_schedule::ScheduleConfig small_fallback_cfg = cfg;
  small_fallback_cfg.fallback_sleep_sec = 10;
  auto plan_reval_zero_now =
      brd_schedule::revalidate_before_sleep(cal, 0, tiny_plan, small_fallback_cfg);
  assert(!plan_reval_zero_now.clock_valid);
  assert(!plan_reval_zero_now.arm_alarm);
  assert(plan_reval_zero_now.sleep_seconds >= brd_schedule::MIN_FALLBACK_SLEEP_SEC);

  auto plan_reval_neg_now =
      brd_schedule::revalidate_before_sleep(cal, -100, tiny_plan, small_fallback_cfg);
  assert(!plan_reval_neg_now.clock_valid);
  assert(plan_reval_neg_now.sleep_seconds >= brd_schedule::MIN_FALLBACK_SLEEP_SEC);

  // Check Revalidation: remaining < 0 (clock jumped forward past target) recomputes.
  auto plan_reval_past =
      brd_schedule::revalidate_before_sleep(cal, plan_spring.target_epoch + 120, plan_spring, cfg);
  assert(plan_reval_past.clock_valid);
  assert(plan_reval_past.target_epoch > plan_spring.target_epoch + 120);
  assert(plan_reval_past.sleep_seconds >= static_cast<uint32_t>(cfg.lead_margin_sec));

  // Check Revalidation: remaining > max_alarm_lead_sec (clock jumped backward) recomputes.
  brd_schedule::SchedulePlan far_future_plan = plan_spring;
  far_future_plan.target_epoch = epoch_spring + 200000;
  auto plan_reval_far = brd_schedule::revalidate_before_sleep(cal, epoch_spring, far_future_plan, cfg);
  assert(plan_reval_far.clock_valid);
  assert(plan_reval_far.target_epoch == plan_spring.target_epoch);
  assert(plan_reval_far.sleep_seconds == plan_spring.sleep_seconds);

  printf("  -> Passed DST and schedule boundary checks.\n");
}

static void test_schedule_dst_edge_cases() {
  printf("[TEST] brd_schedule DST 23:30, 00:15, ambiguous hour, and config bounds...\n");

  brd_schedule::PosixTzCalendar cal("CET-1CEST,M3.5.0,M10.5.0/3");
  brd_schedule::PosixTzCalendar cal_utc;
  brd_schedule::ScheduleConfig cfg;
  cfg.wake_hour = 6;
  cfg.wake_minute = 20;
  cfg.sleep_hour = 21;
  cfg.sleep_minute = 45;
  brd_schedule::WallTime local_target;

  // Check default ScheduleConfig (07:00..22:00) in UTC across daytime and overnight.
  brd_schedule::ScheduleConfig default_cfg;
  int64_t utc_day = 0;
  assert(cal_utc.local_to_epoch({2026, 7, 15, 12, 10, 0}, utc_day));
  const auto plan_utc_day = brd_schedule::plan_next_wake(cal_utc, utc_day, default_cfg);
  assert(plan_utc_day.clock_valid && plan_utc_day.arm_alarm);
  assert(plan_utc_day.target_utc_hour == 12 && plan_utc_day.target_utc_minute == 15);

  int64_t utc_night = 0;
  assert(cal_utc.local_to_epoch({2026, 7, 15, 22, 0, 0}, utc_night));
  const auto plan_utc_night = brd_schedule::plan_next_wake(cal_utc, utc_night, default_cfg);
  assert(plan_utc_night.clock_valid && plan_utc_night.arm_alarm);
  assert(std::strcmp(plan_utc_night.mode, "alarm") == 0);
  assert(plan_utc_night.target_utc_hour == 7 && plan_utc_night.target_utc_minute == 0);

  // Check 21:45 -> 06:20 overnight UTC wake hour/minute in CET (05:20 UTC) and CEST (04:20 UTC).
  int64_t cet_night = 0;
  assert(cal.local_to_epoch({2026, 1, 15, 21, 45, 0}, cet_night));
  const auto plan_cet_night = brd_schedule::plan_next_wake(cal, cet_night, cfg);
  assert(plan_cet_night.arm_alarm);
  assert(plan_cet_night.target_utc_hour == 5 && plan_cet_night.target_utc_minute == 20);

  int64_t cest_night = 0;
  assert(cal.local_to_epoch({2026, 7, 15, 21, 45, 0}, cest_night));
  const auto plan_cest_night = brd_schedule::plan_next_wake(cal, cest_night, cfg);
  assert(plan_cest_night.arm_alarm);
  assert(plan_cest_night.target_utc_hour == 4 && plan_cest_night.target_utc_minute == 20);

  // Check 24h alarm wrap safety margin (diff >= max_alarm_lead_sec disables absolute alarm).
  brd_schedule::ScheduleConfig tight_window_cfg = default_cfg;
  tight_window_cfg.wake_hour = 7;
  tight_window_cfg.wake_minute = 0;
  tight_window_cfg.sleep_hour = 7;
  tight_window_cfg.sleep_minute = 2;
  int64_t near_wrap = 0;
  assert(cal_utc.local_to_epoch({2026, 7, 15, 7, 2, 0}, near_wrap));
  const auto plan_near_wrap = brd_schedule::plan_next_wake(cal_utc, near_wrap, tight_window_cfg);
  assert(plan_near_wrap.clock_valid);
  assert(plan_near_wrap.sleep_seconds == 86280U);
  assert(!plan_near_wrap.arm_alarm);
  const auto reval_near_wrap =
      brd_schedule::revalidate_before_sleep(cal_utc, near_wrap, plan_near_wrap, tight_window_cfg);
  assert(!reval_near_wrap.arm_alarm);

  // Check interval > 60 minutes normalizes cleanly across hours.
  brd_schedule::ScheduleConfig wide_cfg = default_cfg;
  wide_cfg.boundary_interval_min = 90;
  int64_t utc_wide = 0;
  assert(cal_utc.local_to_epoch({2026, 7, 15, 10, 59, 0}, utc_wide));
  const auto plan_wide = brd_schedule::plan_next_wake(cal_utc, utc_wide, wide_cfg);
  assert(plan_wide.clock_valid && plan_wide.arm_alarm);
  assert(plan_wide.target_utc_hour == 11 && plan_wide.target_utc_minute == 30);

  // Check DST spring-forward 23:30 (March 28 -> March 29 06:20 CEST, 23-hour day).
  int64_t spring_eve_2330 = 0;
  assert(cal.local_to_epoch({2026, 3, 28, 23, 30, 0}, spring_eve_2330));
  const auto plan_spring_eve = brd_schedule::plan_next_wake(cal, spring_eve_2330, cfg);
  assert(plan_spring_eve.clock_valid && plan_spring_eve.arm_alarm);
  assert(std::strcmp(plan_spring_eve.mode, "alarm") == 0);
  assert(cal.epoch_to_local(plan_spring_eve.target_epoch, local_target));
  assert(local_target.year == 2026 && local_target.month == 3 && local_target.day == 29);
  assert(local_target.hour == 6 && local_target.minute == 20 && local_target.second == 0);
  assert(plan_spring_eve.sleep_seconds == 21000U);  // 5h50m across 02:00->03:00 jump.

  // Check DST spring-forward evening 23:30 on March 29 -> March 30 06:20 CEST.
  int64_t spring_day_2330 = 0;
  assert(cal.local_to_epoch({2026, 3, 29, 23, 30, 0}, spring_day_2330));
  const auto plan_spring_day_2330 = brd_schedule::plan_next_wake(cal, spring_day_2330, cfg);
  assert(plan_spring_day_2330.clock_valid && plan_spring_day_2330.arm_alarm);
  assert(cal.epoch_to_local(plan_spring_day_2330.target_epoch, local_target));
  assert(local_target.month == 3 && local_target.day == 30);
  assert(local_target.hour == 6 && local_target.minute == 20);
  assert(plan_spring_day_2330.sleep_seconds == 24600U);

  // Check DST fall-back 00:15 (Oct 25, 25-hour day) for both morning wake and next-day advance.
  int64_t fall_0015 = 0;
  assert(cal.local_to_epoch({2026, 10, 25, 0, 15, 0}, fall_0015));
  const auto plan_fall_0015 = brd_schedule::plan_next_wake(cal, fall_0015, cfg);
  assert(plan_fall_0015.clock_valid && plan_fall_0015.arm_alarm);
  assert(cal.epoch_to_local(plan_fall_0015.target_epoch, local_target));
  assert(local_target.month == 10 && local_target.day == 25);
  assert(local_target.hour == 6 && local_target.minute == 20);
  assert(plan_fall_0015.sleep_seconds == 25500U);  // 7h05m across 03:00->02:00 repeat.

  // Check civil day increment when wake_epoch <= now_epoch at 00:15 on 25-hour fall-back day.
  brd_schedule::ScheduleConfig early_wake_cfg = cfg;
  early_wake_cfg.wake_hour = 0;
  early_wake_cfg.wake_minute = 10;
  early_wake_cfg.sleep_hour = 0;
  early_wake_cfg.sleep_minute = 12;
  const auto plan_fall_nextday = brd_schedule::plan_next_wake(cal, fall_0015, early_wake_cfg);
  assert(plan_fall_nextday.clock_valid);
  assert(plan_fall_nextday.target_epoch > fall_0015);
  assert(plan_fall_nextday.sleep_seconds == 89700U);  // 24h55m -> Oct 26 00:10 CET.
  assert(cal.epoch_to_local(plan_fall_nextday.target_epoch, local_target));
  assert(local_target.month == 10 && local_target.day == 26);
  assert(local_target.hour == 0 && local_target.minute == 10);

  // Check DST fall-back ambiguous hour 02:00..02:59 (both CEST and CET occurrences).
  brd_schedule::ScheduleConfig all_day_cfg = default_cfg;
  all_day_cfg.wake_hour = 0;
  all_day_cfg.wake_minute = 0;
  all_day_cfg.sleep_hour = 23;
  all_day_cfg.sleep_minute = 0;
  int64_t fall_ambig_start_utc = 0;
  assert(cal_utc.local_to_epoch({2026, 10, 25, 0, 0, 0}, fall_ambig_start_utc));  // 02:00 CEST
  for (int interval : {15, 5}) {
    all_day_cfg.boundary_interval_min = interval;
    // Sweep every 5 minutes across both 02:00..02:59 CEST (00:00..00:55 UTC) and CET (01:00..01:55 UTC).
    for (int mins = 0; mins < 120; mins += 5) {
      const int64_t now_ep = fall_ambig_start_utc + static_cast<int64_t>(mins) * 60;
      const auto p = brd_schedule::plan_next_wake(cal, now_ep, all_day_cfg);
      assert(p.clock_valid);
      assert(p.target_epoch > now_ep);
      assert(p.sleep_seconds >= static_cast<uint32_t>(all_day_cfg.lead_margin_sec));
    }
  }

  // Check small interval (1 min) with large lead margin (300 s) steps forward safely.
  brd_schedule::ScheduleConfig small_int_cfg = default_cfg;
  small_int_cfg.boundary_interval_min = 1;
  small_int_cfg.lead_margin_sec = 300;
  const auto plan_small_int = brd_schedule::plan_next_wake(cal_utc, utc_day, small_int_cfg);
  assert(plan_small_int.clock_valid);
  assert(plan_small_int.sleep_seconds >= 300U);

  // Check plan_next_wake fails closed when MAX_BOUNDARY_STEPS cannot reach lead_margin_sec.
  brd_schedule::ScheduleConfig exhausted_cfg = default_cfg;
  exhausted_cfg.boundary_interval_min = 1;
  exhausted_cfg.lead_margin_sec = 4000;
  exhausted_cfg.fallback_sleep_sec = 900;
  const auto plan_exhausted = brd_schedule::plan_next_wake(cal_utc, utc_day, exhausted_cfg);
  assert(!plan_exhausted.clock_valid && !plan_exhausted.arm_alarm);
  assert(plan_exhausted.target_epoch == 0);
  assert(plan_exhausted.sleep_seconds == 900U);

  // Check invalid ScheduleConfig fields return clamped fallback plan.
  brd_schedule::ScheduleConfig invalid_cfg = default_cfg;
  invalid_cfg.fallback_sleep_sec = 10;
  invalid_cfg.wake_hour = 24;
  assert(!brd_schedule::is_valid_schedule_config(invalid_cfg));
  auto plan_bad_cfg = brd_schedule::plan_next_wake(cal_utc, utc_day, invalid_cfg);
  assert(!plan_bad_cfg.clock_valid && !plan_bad_cfg.arm_alarm);
  assert(plan_bad_cfg.sleep_seconds == brd_schedule::MIN_FALLBACK_SLEEP_SEC);

  invalid_cfg = default_cfg;
  invalid_cfg.sleep_hour = -1;
  assert(!brd_schedule::is_valid_schedule_config(invalid_cfg));
  plan_bad_cfg = brd_schedule::plan_next_wake(cal_utc, utc_day, invalid_cfg);
  assert(!plan_bad_cfg.clock_valid);

  invalid_cfg = default_cfg;
  invalid_cfg.wake_minute = 60;
  assert(!brd_schedule::is_valid_schedule_config(invalid_cfg));
  plan_bad_cfg = brd_schedule::plan_next_wake(cal_utc, utc_day, invalid_cfg);
  assert(!plan_bad_cfg.clock_valid);

  invalid_cfg = default_cfg;
  invalid_cfg.sleep_minute = 60;
  assert(!brd_schedule::is_valid_schedule_config(invalid_cfg));
  plan_bad_cfg = brd_schedule::plan_next_wake(cal_utc, utc_day, invalid_cfg);
  assert(!plan_bad_cfg.clock_valid);

  // Check is_valid_schedule_config rejects sleep_sec <= wake_sec.
  invalid_cfg = default_cfg;
  invalid_cfg.wake_hour = 7;
  invalid_cfg.wake_minute = 0;
  invalid_cfg.sleep_hour = 7;
  invalid_cfg.sleep_minute = 0;
  assert(!brd_schedule::is_valid_schedule_config(invalid_cfg));
  plan_bad_cfg = brd_schedule::plan_next_wake(cal_utc, utc_day, invalid_cfg);
  assert(!plan_bad_cfg.clock_valid && !plan_bad_cfg.arm_alarm);

  invalid_cfg.wake_hour = 22;
  invalid_cfg.wake_minute = 0;
  invalid_cfg.sleep_hour = 7;
  invalid_cfg.sleep_minute = 0;
  assert(!brd_schedule::is_valid_schedule_config(invalid_cfg));
  plan_bad_cfg = brd_schedule::plan_next_wake(cal_utc, utc_day, invalid_cfg);
  assert(!plan_bad_cfg.clock_valid && !plan_bad_cfg.arm_alarm);

  invalid_cfg.wake_hour = 7;
  invalid_cfg.wake_minute = 30;
  invalid_cfg.sleep_hour = 7;
  invalid_cfg.sleep_minute = 15;
  assert(!brd_schedule::is_valid_schedule_config(invalid_cfg));
  plan_bad_cfg = brd_schedule::plan_next_wake(cal_utc, utc_day, invalid_cfg);
  assert(!plan_bad_cfg.clock_valid && !plan_bad_cfg.arm_alarm);

  printf("  -> Passed DST and schedule boundary checks.\n");
}

static void test_rtc_bcd_and_retries() {
  printf("[TEST] brd_rtc BCD codec and I2C operations...\n");

  // BCD conversion table test across all 0..99 values.
  for (uint8_t i = 0; i < 100; ++i) {
    uint8_t bcd = brd_rtc::dec_to_bcd(i);
    uint8_t dec = brd_rtc::bcd_to_dec(bcd);
    assert(dec == i);
  }

  // Check compute_esphome_weekday across Sunday, Saturday, Monday, leap day, and invalid dates.
  assert(brd_rtc::compute_esphome_weekday(2026, 11, 15) == 1);  // Sunday
  assert(brd_rtc::compute_esphome_weekday(2026, 11, 16) == 2);  // Monday
  assert(brd_rtc::compute_esphome_weekday(2026, 11, 14) == 7);  // Saturday
  assert(brd_rtc::compute_esphome_weekday(2024, 2, 29) == 5);   // Leap Thursday
  assert(brd_rtc::compute_esphome_weekday(2026, 0, 15) == 0);
  assert(brd_rtc::compute_esphome_weekday(2026, 13, 15) == 0);
  assert(brd_rtc::compute_esphome_weekday(2026, 11, 0) == 0);
  assert(brd_rtc::compute_esphome_weekday(2026, 2, 30) == 0);
  assert(brd_rtc::compute_esphome_weekday(2025, 2, 29) == 0);
  assert(brd_rtc::compute_esphome_weekday(2026, 4, 31) == 0);

  MockI2cBus mock;

  // Check MockI2cBus bounds guards reject out-of-range register writes and reads.
  const uint8_t oob_write[4] = {0xFE, 0x01, 0x02, 0x03};
  assert(!mock.write(0x51, oob_write, sizeof(oob_write)));
  uint8_t oob_read[4]{};
  const uint8_t oob_reg = 0xFE;
  assert(!mock.write_read(0x51, &oob_reg, 1, oob_read, sizeof(oob_read)));

  // Check 3-attempt bounded retry success.
  mock.fail_count = 2;
  const uint8_t test_data[2] = {0x01, 0x00};
  assert(brd_rtc::retry_write(mock, 0x51, test_data, 2));

  // Check 3-attempt bounded retry exhaustion.
  mock.fail_count = 3;
  assert(!brd_rtc::retry_write(mock, 0x51, test_data, 2));

  // Check read_and_clear_status_and_disable_clkout clears Control_status_1 (STOP/TEST), 0x01, and 0x0D.
  mock.fail_count = 0;
  mock.registers[0x00] = 0xA8;  // TEST1 + STOP + TESTC bits set
  mock.registers[0x01] = 0x0C;
  mock.registers[0x0D] = 0x80;
  brd_rtc::RtcStatus status;
  assert(brd_rtc::read_and_clear_status_and_disable_clkout(mock, 0x51, status));
  assert(status.alarm_flag && status.timer_flag);
  assert(mock.registers[0x00] == 0x00);
  assert(mock.registers[0x01] == 0x00);
  assert(mock.registers[0x0D] == 0x00);

  // Check write_utc_clock_only writes ESPTime weekday 1..7 (2026-11-16 is Monday -> 2).
  brd_rtc::UtcClock clk;
  clk.year = 2026;
  clk.month = 11;
  clk.day = 16;
  clk.weekday = 0;
  clk.hour = 12;
  clk.minute = 34;
  clk.second = 56;
  assert(brd_rtc::write_utc_clock_only(mock, 0x51, clk));
  assert(mock.registers[0x02] == brd_rtc::dec_to_bcd(56));
  assert(mock.registers[0x03] == brd_rtc::dec_to_bcd(34));
  assert(mock.registers[0x04] == brd_rtc::dec_to_bcd(12));
  assert(mock.registers[0x05] == brd_rtc::dec_to_bcd(16));
  assert(mock.registers[0x06] == brd_rtc::dec_to_bcd(2));
  assert(mock.registers[0x07] == brd_rtc::dec_to_bcd(11));
  assert(mock.registers[0x08] == brd_rtc::dec_to_bcd(26));
  assert(mock.registers[0x0D] == 0x00);

  // Check write_utc_clock_only writes 1 for Sunday, clears VL bit, and clears STOP bit.
  mock.registers[0x00] = 0x20;  // STOP=1
  clk.year = 2026;
  clk.month = 11;
  clk.day = 15;
  clk.weekday = 0;
  assert(brd_rtc::write_utc_clock_only(mock, 0x51, clk));
  assert(mock.registers[0x00] == 0x00);
  assert(mock.registers[0x06] == 1);
  assert((mock.registers[0x02] & 0x80) == 0);

  // Check write_utc_clock_only rejects out-of-range fields and impossible calendar dates.
  brd_rtc::UtcClock bad_clk = clk;
  bad_clk.year = 1999;
  assert(!brd_rtc::write_utc_clock_only(mock, 0x51, bad_clk));
  bad_clk = clk;
  bad_clk.hour = 24;
  assert(!brd_rtc::write_utc_clock_only(mock, 0x51, bad_clk));
  bad_clk = clk;
  bad_clk.minute = 60;
  assert(!brd_rtc::write_utc_clock_only(mock, 0x51, bad_clk));
  bad_clk = clk;
  bad_clk.second = 60;
  assert(!brd_rtc::write_utc_clock_only(mock, 0x51, bad_clk));
  bad_clk = clk;
  bad_clk.year = 2026;
  bad_clk.month = 2;
  bad_clk.day = 30;
  assert(!brd_rtc::write_utc_clock_only(mock, 0x51, bad_clk));
  bad_clk.year = 2025;
  bad_clk.month = 2;
  bad_clk.day = 29;
  assert(!brd_rtc::write_utc_clock_only(mock, 0x51, bad_clk));

  // Check repair_sunday_weekday_if_valid repairs 0 -> 1 when VL == 0 on Sunday 2026-11-15.
  mock.registers[0x02] = brd_rtc::dec_to_bcd(10);  // VL == 0
  mock.registers[0x05] = brd_rtc::dec_to_bcd(15);
  mock.registers[0x06] = 0;  // Midnight rollover from Saturday 7 -> 0
  mock.registers[0x07] = brd_rtc::dec_to_bcd(11);
  mock.registers[0x08] = brd_rtc::dec_to_bcd(26);
  assert(brd_rtc::repair_sunday_weekday_if_valid(mock, 0x51));
  assert(mock.registers[0x06] == 1);

  // Check repair_sunday_weekday_if_valid skips invalid calendar date (2026-02-30).
  mock.registers[0x02] = brd_rtc::dec_to_bcd(10);
  mock.registers[0x05] = brd_rtc::dec_to_bcd(30);
  mock.registers[0x06] = 0;
  mock.registers[0x07] = brd_rtc::dec_to_bcd(2);
  mock.registers[0x08] = brd_rtc::dec_to_bcd(26);
  assert(brd_rtc::repair_sunday_weekday_if_valid(mock, 0x51));
  assert(mock.registers[0x06] == 0);

  // Check repair_sunday_weekday_if_valid leaves register untouched when VL == 1.
  mock.registers[0x02] = static_cast<uint8_t>(0x80 | brd_rtc::dec_to_bcd(10));  // VL == 1
  mock.registers[0x05] = brd_rtc::dec_to_bcd(15);
  mock.registers[0x06] = 0;
  mock.registers[0x07] = brd_rtc::dec_to_bcd(11);
  mock.registers[0x08] = brd_rtc::dec_to_bcd(26);
  assert(brd_rtc::repair_sunday_weekday_if_valid(mock, 0x51));
  assert(mock.registers[0x06] == 0);

  printf("  -> Passed BCD and RTC calendar checks.\n");
}

static void test_rtc_arm_wake_protocol() {
  printf("[TEST] brd_rtc arm_wake protocol, validation, and readback checks...\n");

  MockI2cBus mock;

  // Check arm_wake rejects invalid alarm hour (>= 24) and minute (>= 60).
  brd_rtc::WakeSettings bad_alarm;
  bad_alarm.use_alarm = true;
  bad_alarm.target_utc_hour = 24;
  bad_alarm.target_utc_minute = 0;
  assert(!brd_rtc::arm_wake(mock, 0x51, bad_alarm));
  bad_alarm.target_utc_hour = 23;
  bad_alarm.target_utc_minute = 60;
  assert(!brd_rtc::arm_wake(mock, 0x51, bad_alarm));
  bad_alarm.target_utc_hour = 255;
  bad_alarm.target_utc_minute = 255;
  assert(!brd_rtc::arm_wake(mock, 0x51, bad_alarm));

  // Check arm_wake in alarm mode.
  brd_rtc::WakeSettings ws_alarm;
  ws_alarm.use_alarm = true;
  ws_alarm.target_utc_hour = 5;
  ws_alarm.target_utc_minute = 20;
  ws_alarm.sleep_seconds = 28800;
  assert(brd_rtc::arm_wake(mock, 0x51, ws_alarm));
  assert((mock.registers[0x01] & 0x02) != 0);
  assert(mock.registers[0x09] == brd_rtc::dec_to_bcd(20));
  assert(mock.registers[0x0A] == brd_rtc::dec_to_bcd(5));
  assert(mock.registers[0x0B] == 0x80);
  assert(mock.registers[0x0C] == 0x80);

  // Check alarm readback allows unused bit 6 (0x40) on 0x0A.
  mock.clear_read_overrides();
  mock.override_read[0x0A] = true;
  mock.override_val[0x0A] = static_cast<uint8_t>(0x40U | brd_rtc::dec_to_bcd(5));
  assert(brd_rtc::arm_wake(mock, 0x51, ws_alarm));

  // Check alarm readback rejects AE bit (0x80) set on 0x09 or 0x0A and disarms RTC.
  mock.clear_read_overrides();
  mock.override_read[0x09] = true;
  mock.override_val[0x09] = static_cast<uint8_t>(0x80 | brd_rtc::dec_to_bcd(20));
  assert(!brd_rtc::arm_wake(mock, 0x51, ws_alarm));
  assert(mock.registers[0x0E] == 0x00 && mock.registers[0x01] == 0x00);
  mock.clear_read_overrides();
  mock.override_read[0x0A] = true;
  mock.override_val[0x0A] = static_cast<uint8_t>(0x80 | brd_rtc::dec_to_bcd(5));
  assert(!brd_rtc::arm_wake(mock, 0x51, ws_alarm));
  assert(mock.registers[0x0E] == 0x00 && mock.registers[0x01] == 0x00);
  mock.override_val[0x0A] = static_cast<uint8_t>(0xC0U | brd_rtc::dec_to_bcd(5));
  assert(!brd_rtc::arm_wake(mock, 0x51, ws_alarm));
  assert(mock.registers[0x0E] == 0x00 && mock.registers[0x01] == 0x00);

  // Check alarm readback rejects cleared AE bit (bit 7 == 0) on 0x0B or 0x0C and disarms RTC.
  mock.clear_read_overrides();
  mock.override_read[0x0B] = true;
  mock.override_val[0x0B] = 0x00;
  assert(!brd_rtc::arm_wake(mock, 0x51, ws_alarm));
  assert(mock.registers[0x0E] == 0x00 && mock.registers[0x01] == 0x00);
  mock.clear_read_overrides();
  mock.override_read[0x0C] = true;
  mock.override_val[0x0C] = 0x00;
  assert(!brd_rtc::arm_wake(mock, 0x51, ws_alarm));
  assert(mock.registers[0x0E] == 0x00 && mock.registers[0x01] == 0x00);

  // Check alarm readback rejects AF or TF latched in register 0x01 and disarms RTC.
  mock.clear_read_overrides();
  mock.override_read[0x01] = true;
  mock.override_val[0x01] = 0x0A;  // AIE | AF
  assert(!brd_rtc::arm_wake(mock, 0x51, ws_alarm));
  assert(mock.registers[0x0E] == 0x00 && mock.registers[0x01] == 0x00);
  mock.clear_read_overrides();

  // Check arm_wake readback mismatch disarms RTC and returns false.
  mock.inject_readback_mismatch = true;
  assert(!brd_rtc::arm_wake(mock, 0x51, ws_alarm));
  assert(mock.registers[0x0E] == 0x00 && mock.registers[0x01] == 0x00);
  mock.inject_readback_mismatch = false;

  // Check alarm readback I2C failure disarms RTC before returning false.
  mock.fail_read_count = 3;
  assert(!brd_rtc::arm_wake(mock, 0x51, ws_alarm));
  assert(mock.registers[0x0E] == 0x00 && mock.registers[0x01] == 0x00);
  mock.fail_read_count = 0;

  // Check arm_wake in timer mode across boundary sleep_seconds values.
  struct TimerCase {
    uint32_t sleep_seconds;
    uint8_t expected_ctrl;
    uint8_t expected_val;
  };
  constexpr TimerCase TIMER_CASES[] = {
      {0U, 0x82, 1},
      {120U, 0x82, 120},
      {255U, 0x82, 255},
      {256U, 0x83, 4},
      {898U, 0x83, 15},
      {15300U, 0x83, 255},
      {std::numeric_limits<uint32_t>::max(), 0x83, 255},
  };
  for (const auto &tc : TIMER_CASES) {
    mock.history.clear();
    brd_rtc::WakeSettings ws_timer;
    ws_timer.use_alarm = false;
    ws_timer.sleep_seconds = tc.sleep_seconds;
    assert(brd_rtc::arm_wake(mock, 0x51, ws_timer));
    assert(mock.registers[0x01] == 0x01);
    assert(mock.registers[0x0E] == tc.expected_ctrl);
    assert(mock.registers[0x0F] == tc.expected_val);
    assert(mock.registers[0x09] == 0x80 && mock.registers[0x0A] == 0x80);
    assert(mock.registers[0x0B] == 0x80 && mock.registers[0x0C] == 0x80);

    // Check timer sequence: prep 0x0E -> write 0x0F -> read 0x0F -> write 0x01 -> start 0x0E -> read 0x0E -> read 0x01.
    int idx_prep = -1;
    int idx_val = -1;
    int idx_rb_0f = -1;
    int idx_tie = -1;
    int idx_start = -1;
    int idx_rb_0e = -1;
    int idx_rb_01 = -1;
    for (size_t i = 0; i < mock.history.size(); ++i) {
      const auto &op = mock.history[i];
      if (op.is_read) {
        if (op.write_data.size() == 1 && op.write_data[0] == 0x0F) {
          idx_rb_0f = static_cast<int>(i);
        } else if (op.write_data.size() == 1 && op.write_data[0] == 0x0E) {
          idx_rb_0e = static_cast<int>(i);
        } else if (op.write_data.size() == 1 && op.write_data[0] == 0x01) {
          idx_rb_01 = static_cast<int>(i);
        }
        continue;
      }
      if (op.write_data.size() < 2) {
        continue;
      }
      if (op.write_data[0] == 0x0E && op.write_data[1] == (tc.expected_ctrl & 0x7FU)) {
        idx_prep = static_cast<int>(i);
      } else if (op.write_data[0] == 0x0F && op.write_data[1] == tc.expected_val) {
        idx_val = static_cast<int>(i);
      } else if (op.write_data[0] == 0x01 && op.write_data[1] == 0x01) {
        idx_tie = static_cast<int>(i);
      } else if (op.write_data[0] == 0x0E && op.write_data[1] == tc.expected_ctrl) {
        idx_start = static_cast<int>(i);
      }
    }
    assert(idx_prep >= 0 && idx_val > idx_prep && idx_rb_0f > idx_val && idx_tie > idx_rb_0f &&
           idx_start > idx_tie && idx_rb_0e > idx_start && idx_rb_01 > idx_rb_0e);
  }

  // Check timer arm succeeds even if 0x0F decrements immediately when TE becomes 1.
  brd_rtc::WakeSettings ws_timer_check;
  ws_timer_check.use_alarm = false;
  ws_timer_check.sleep_seconds = 900;
  mock.clear_read_overrides();
  mock.decrement_timer_on_start = true;
  assert(brd_rtc::arm_wake(mock, 0x51, ws_timer_check));
  assert(mock.registers[0x0F] == 14U);
  mock.decrement_timer_on_start = false;

  // Check timer readback allows unused bits 6 through 2 (0x7C) on 0x0E.
  mock.clear_read_overrides();
  mock.override_read[0x0E] = true;
  mock.override_val[0x0E] = static_cast<uint8_t>(0x7CU | 0x83U);
  assert(brd_rtc::arm_wake(mock, 0x51, ws_timer_check));

  // Check timer readback rejects 0x0E mismatch, 0x0F mismatch, AF set, and TF set and disarms RTC.
  mock.clear_read_overrides();
  mock.override_read[0x0E] = true;
  mock.override_val[0x0E] = static_cast<uint8_t>(0x7CU | 0x82U);  // Wrong frequency bits
  assert(!brd_rtc::arm_wake(mock, 0x51, ws_timer_check));
  assert(mock.registers[0x0E] == 0x00 && mock.registers[0x01] == 0x00);

  mock.clear_read_overrides();
  mock.override_read[0x0F] = true;
  mock.override_val[0x0F] = 14;  // Wrong countdown value
  assert(!brd_rtc::arm_wake(mock, 0x51, ws_timer_check));
  assert(mock.registers[0x0E] == 0x00 && mock.registers[0x01] == 0x00);

  mock.clear_read_overrides();
  mock.override_read[0x01] = true;
  mock.override_val[0x01] = 0x09;  // TIE | AF
  assert(!brd_rtc::arm_wake(mock, 0x51, ws_timer_check));
  assert(mock.registers[0x0E] == 0x00 && mock.registers[0x01] == 0x00);

  mock.clear_read_overrides();
  mock.override_read[0x01] = true;
  mock.override_val[0x01] = 0x05;  // TIE | TF
  assert(!brd_rtc::arm_wake(mock, 0x51, ws_timer_check));
  assert(mock.registers[0x0E] == 0x00 && mock.registers[0x01] == 0x00);
  mock.clear_read_overrides();

  // Check timer readback I2C failure disarms RTC before returning false.
  mock.fail_read_count = 3;
  assert(!brd_rtc::arm_wake(mock, 0x51, ws_timer_check));
  assert(mock.registers[0x0E] == 0x00 && mock.registers[0x01] == 0x00);
  mock.fail_read_count = 0;

  printf("  -> Passed BCD and RTC driver checks.\n");
}

static void test_power_state_and_telemetry() {
  printf("[TEST] brd_power state CRC, classification, USB hysteresis, and telemetry...\n");

  // Test voltage_to_mv conversion and boundary guards.
  assert(brd_power::voltage_to_mv(std::numeric_limits<float>::quiet_NaN()) == 0U);
  assert(brd_power::voltage_to_mv(-1.0f) == 0U);
  assert(brd_power::voltage_to_mv(0.0f) == 0U);
  assert(brd_power::voltage_to_mv(4.196f) == 4196U);
  assert(brd_power::voltage_to_mv(15.0f) == 0U);

  // Shift register bit constants.
  assert(brd_power::CFG_RAILS_ONLY_ON == 0x22);
  assert(brd_power::CFG_ALL_OFF_SMPS_DISABLED == 0x02);

  // Table-driven tests for battery_voltage_to_percent.
  assert(brd_power::battery_voltage_to_percent(std::numeric_limits<float>::quiet_NaN()) == 0.0f);
  assert(brd_power::battery_voltage_to_percent(std::numeric_limits<float>::infinity()) == 0.0f);
  assert(brd_power::battery_voltage_to_percent(-std::numeric_limits<float>::infinity()) == 0.0f);
  assert(brd_power::battery_voltage_to_percent(3.70f, 3.70f, 3.70f) == 0.0f);
  assert(brd_power::battery_voltage_to_percent(3.70f, 4.20f, 3.00f) == 0.0f);
  assert(brd_power::battery_voltage_to_percent(3.70f, std::numeric_limits<float>::quiet_NaN(), 4.20f) == 0.0f);
  assert(brd_power::battery_voltage_to_percent(3.70f, 3.00f, std::numeric_limits<float>::quiet_NaN()) == 0.0f);

  // Clamping below minimum discharge threshold.
  assert(brd_power::battery_voltage_to_percent(-1.00f) == 0.0f);
  assert(brd_power::battery_voltage_to_percent(0.00f) == 0.0f);
  assert(brd_power::battery_voltage_to_percent(2.50f) == 0.0f);
  assert(brd_power::battery_voltage_to_percent(2.99f) == 0.0f);

  // Clamping above full charge threshold.
  assert(brd_power::battery_voltage_to_percent(4.20f) == 100.0f);
  assert(brd_power::battery_voltage_to_percent(4.21f) == 100.0f);
  assert(brd_power::battery_voltage_to_percent(4.50f) == 100.0f);
  assert(brd_power::battery_voltage_to_percent(5.00f) == 100.0f);

  // Exact curve points matching upstream battery driver.
  struct BatteryCurvePoint {
    float voltage;
    float expected_percent;
  };
  const BatteryCurvePoint curve_points[] = {
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
  };
  for (const auto &pt : curve_points) {
    float actual = brd_power::battery_voltage_to_percent(pt.voltage);
    assert(std::fabs(actual - pt.expected_percent) < 0.01f);
  }

  // Linear interpolation midpoints.
  assert(std::fabs(brd_power::battery_voltage_to_percent(4.15f) - 96.0f) < 0.01f);
  assert(std::fabs(brd_power::battery_voltage_to_percent(3.15f) - 2.0f) < 0.01f);

  // Monotonicity verification from 3.00 V to 4.20 V.
  float prev_pct = -1.0f;
  for (float v = 3.00f; v <= 4.201f; v += 0.005f) {
    float pct = brd_power::battery_voltage_to_percent(v);
    assert(pct >= prev_pct);
    assert(pct >= 0.0f && pct <= 100.0f);
    prev_pct = pct;
  }

  // Touch point coordinate bounds verification (landscape 960x540 and portrait 540x960).
  assert(brd_power::is_valid_touch_point(0, 0, 960, 540));
  assert(brd_power::is_valid_touch_point(959, 539, 960, 540));
  assert(brd_power::is_valid_touch_point(480, 270, 960, 540));
  assert(!brd_power::is_valid_touch_point(-1, 0, 960, 540));
  assert(!brd_power::is_valid_touch_point(0, -1, 960, 540));
  assert(!brd_power::is_valid_touch_point(960, 0, 960, 540));
  assert(!brd_power::is_valid_touch_point(0, 540, 960, 540));
  assert(!brd_power::is_valid_touch_point(960, 540, 960, 540));
  assert(!brd_power::is_valid_touch_point(1000, 200, 960, 540));
  assert(!brd_power::is_valid_touch_point(500, 600, 960, 540));
  assert(!brd_power::is_valid_touch_point(-100, -100, 960, 540));

  // Portrait 540x960 bounds and invalid display dimension checks.
  assert(brd_power::is_valid_touch_point(200, 750, 540, 960));
  assert(brd_power::is_valid_touch_point(539, 959, 540, 960));
  assert(!brd_power::is_valid_touch_point(540, 500, 540, 960));
  assert(!brd_power::is_valid_touch_point(200, 960, 540, 960));
  assert(!brd_power::is_valid_touch_point(10, 10, 0, 960));
  assert(!brd_power::is_valid_touch_point(10, 10, 540, 0));
  assert(!brd_power::is_valid_touch_point(10, 10, -540, 960));
  assert(!brd_power::is_valid_touch_point(10, 10, 540, -960));

  printf("  -> Passed battery curve and touch bounds checks.\n");
}

static void test_power_state_and_usb() {
  printf("[TEST] brd_power RTC state CRC, sleep locks, and USB hysteresis...\n");

  // RtcPersistentState CRC validation, version check, and padding determinism.
  brd_power::RtcPersistentState state_aa;
  brd_power::RtcPersistentState state_55;
  std::memset(static_cast<void *>(&state_aa), 0xAA, sizeof(state_aa));
  std::memset(static_cast<void *>(&state_55), 0x55, sizeof(state_55));
  state_aa.reset();
  state_55.reset();
  assert(std::memcmp(&state_aa, &state_55, sizeof(state_aa)) == 0);
  assert(state_aa.crc32 == state_55.crc32);
  assert(state_aa.version == brd_power::RTC_STATE_VERSION);
  assert(state_aa.validate_or_reset());

  // Version mismatch triggers reset even with matching CRC.
  state_aa.version = static_cast<uint16_t>(brd_power::RTC_STATE_VERSION + 1U);
  state_aa.update_crc();
  assert(!state_aa.validate_or_reset());
  assert(state_aa.version == brd_power::RTC_STATE_VERSION);

  brd_power::RtcPersistentState state;
  state.reset();
  assert(state.validate_or_reset());
  assert(state.boot_count == 0);
  assert(state.magic == brd_power::RTC_STATE_MAGIC);

  state.boot_count = 42;
  state.prev_batt_mv = 4120;
  state.staying_awake_usb = true;
  state.prev_gt911 = 2;
  state.last_gt911_addr = 2;
  state.update_crc();
  assert(state.validate_or_reset());
  assert(state.boot_count == 42);
  assert(state.last_gt911_addr == 2);

  // Clearing telemetry prev_gt911 preserves cached last_gt911_addr.
  state.prev_gt911 = -1;
  state.update_crc();
  assert(state.validate_or_reset());
  assert(state.prev_gt911 == -1);
  assert(state.last_gt911_addr == 2);

  // Corrupt state single-bit triggers auto-reset.
  state.boot_count ^= 0x01;
  assert(!state.validate_or_reset());
  assert(state.boot_count == 0);
  assert(state.last_gt911_addr == 0);
  assert(state.validate_or_reset());

  // Sleep lock reference counting and sleep eligibility predicate.
  brd_power::sleep_lock_count = 0;
  assert(!brd_power::is_sleep_locked());
  brd_power::release_sleep_lock();  // Underflow stays clamped at 0.
  assert(!brd_power::is_sleep_locked());
  brd_power::acquire_sleep_lock();
  brd_power::acquire_sleep_lock();
  assert(brd_power::is_sleep_locked() && brd_power::sleep_lock_count == 2U);
  brd_power::release_sleep_lock();
  assert(brd_power::is_sleep_locked() && brd_power::sleep_lock_count == 1U);
  brd_power::release_sleep_lock();
  assert(!brd_power::is_sleep_locked() && brd_power::sleep_lock_count == 0U);

  assert(brd_power::can_enter_sleep(false, false, false, 0U));
  assert(!brd_power::can_enter_sleep(true, false, false, 0U));
  assert(!brd_power::can_enter_sleep(false, true, false, 0U));
  assert(!brd_power::can_enter_sleep(false, false, true, 0U));
  assert(!brd_power::can_enter_sleep(false, false, false, 1U));

  // Wake classification table tests.
  assert(std::strcmp(brd_power::classify_wake_reason(3, (1ULL << 21), false, false), "Ext_BTN") == 0);
  assert(std::strcmp(brd_power::classify_wake_reason(3, (1ULL << 9), false, false), "Ext_RTC") == 0);
  assert(std::strcmp(brd_power::classify_wake_reason(3, 0, true, false), "Ext_RTC") == 0);
  assert(std::strcmp(brd_power::classify_wake_reason(3, 0, false, true), "Ext_RTC") == 0);
  assert(std::strcmp(brd_power::classify_wake_reason(4, 0, false, false), "Int_RTC") == 0);
  assert(std::strcmp(brd_power::classify_wake_reason(0, 0, false, false), "Power_On") == 0);

  // USB power evaluation tests across all 5 conditions:
  // 1. SOF increment returns true unconditionally.
  assert(brd_power::evaluate_usb_power(0, 0, false, true, false));
  assert(brd_power::evaluate_usb_power(3500, 3500, false, true, true));

  // 2. Currently awake returns true when voltage >= 4185 mV.
  assert(brd_power::evaluate_usb_power(4185, 4200, true, false, false));
  assert(!brd_power::evaluate_usb_power(4184, 4200, true, false, false));

  // 3. Voltage rise >= 25 mV returns true without uint32_t addition overflow.
  assert(brd_power::evaluate_usb_power(4175, 4150, false, false, false));
  assert(!brd_power::evaluate_usb_power(4174, 4150, false, false, false));
  assert(!brd_power::evaluate_usb_power(100U, std::numeric_limits<uint32_t>::max() - 10U, false,
                                        false, false));
  assert(brd_power::evaluate_usb_power(std::numeric_limits<uint32_t>::max(),
                                       std::numeric_limits<uint32_t>::max() - 25U, false, false,
                                       false));
  assert(!brd_power::evaluate_usb_power(std::numeric_limits<uint32_t>::max(),
                                        std::numeric_limits<uint32_t>::max() - 24U, false, false,
                                        false));

  // 4. Cold boot with voltage >= 4195 mV returns true.
  assert(brd_power::evaluate_usb_power(4195, 0, false, false, false));
  assert(!brd_power::evaluate_usb_power(4194, 0, false, false, false));
  // Not a cold boot when prev_slept is true.
  assert(!brd_power::evaluate_usb_power(4195, 0, false, false, true));

  // 5. None of the above returns false.
  assert(!brd_power::evaluate_usb_power(4100, 4100, false, false, false));

  // Table-driven tests for RTC/NTP sync helpers.
  struct SyncDueCase {
    bool rtc_valid;
    uint32_t current_epoch;
    uint32_t last_sync_epoch;
    bool expected_due;
  };
  constexpr SyncDueCase SYNC_DUE_CASES[] = {
      {false, 1795500000U, 1795499900U, true},   // Invalid RTC clock
      {true, 1795500000U, 0U, true},             // Never synced
      {true, 1795499999U, 1795500000U, true},    // Clock rolled back
      {true, 1795500000U, 1795500000U, false},   // Just synced
      {true, 1795521599U, 1795500000U, false},   // 1 second before 6h interval
      {true, 1795521600U, 1795500000U, true},    // Exact 6h boundary
      {true, 1795530000U, 1795500000U, true},    // Past 6h interval
  };
  for (const auto &tc : SYNC_DUE_CASES) {
    assert(brd_power::is_rtc_sync_due(tc.rtc_valid, tc.current_epoch, tc.last_sync_epoch) ==
           tc.expected_due);
  }

  struct SyncStatusCase {
    bool requested;
    bool ntp_synced;
    bool rtc_saved;
    const char *expected_status;
    int expected_metric;
  };
  constexpr SyncStatusCase SYNC_STATUS_CASES[] = {
      {false, false, false, "skipped", 0},
      {false, false, true, "skipped", 0},
      {true, false, false, "timeout", -1},
      {true, false, true, "timeout", -1},
      {false, true, false, "save_err", -1},
      {true, true, false, "save_err", -1},
      {false, true, true, "synced", 1},
      {true, true, true, "synced", 1},
  };
  for (const auto &tc : SYNC_STATUS_CASES) {
    assert(std::strcmp(brd_power::classify_rtc_sync_status(tc.requested, tc.ntp_synced, tc.rtc_saved),
                       tc.expected_status) == 0);
    assert(brd_power::classify_rtc_sync_metric(tc.requested, tc.ntp_synced, tc.rtc_saved) ==
           tc.expected_metric);
  }

  assert(brd_power::compute_rtc_sync_age(0U, 1795500000U) == -1);
  assert(brd_power::compute_rtc_sync_age(1795500000U, 0U) == -1);
  assert(brd_power::compute_rtc_sync_age(1795499999U, 1795500000U) == -1);
  assert(brd_power::compute_rtc_sync_age(1795500000U, 1795500000U) == 0);
  assert(brd_power::compute_rtc_sync_age(1795500900U, 1795500000U) == 900);
  // Large 32-bit epoch difference stays positive in int64_t.
  assert(brd_power::compute_rtc_sync_age(4000000000U, 1000000000U) == 3000000000LL);

  printf("  -> Passed power state and USB checks.\n");
}

static void test_power_telemetry() {
  printf("[TEST] brd_power vlogs and Prometheus telemetry formatters...\n");

  // Telemetry formatters.
  brd_power::TraceTelemetry telem;
  telem.wake_reason = "Ext_RTC";
  telem.mode = "timer";
  telem.batt_v = 4.12f;
  telem.batt_pct = 92.0f;
  telem.awake_s = 2.4f;
  telem.image_status = "new";
  telem.touch_x = -1;
  telem.touch_y = -1;
  telem.now_str = "2026-11-16T01:30:00+0100";
  telem.next_str = "2026-11-16T01:45:00+0100";
  telem.next_in_str = "900";
  telem.since_str = "900";
  telem.prev_slept = true;
  telem.prev_gt911 = 1;
  telem.boot_count = 15;
  telem.job_name = "brd";
  telem.since_num = 900;
  telem.rtc_sync = "synced";
  telem.rtc_sync_code = 1;
  telem.rtc_sync_age_sec = 0;

  char vlogs_msg[384];
  int vlen = brd_power::format_vlogs_message(vlogs_msg, sizeof(vlogs_msg), telem);
  assert(vlen > 0 && static_cast<size_t>(vlen) < sizeof(vlogs_msg));
  assert(std::strstr(vlogs_msg, "wake=Ext_RTC mode=timer batt=4.12V batt_pct=92% awake=2.4s image=new touch=none") != nullptr);
  assert(std::strstr(vlogs_msg, "next_in=900s since=900s") != nullptr);
  assert(std::strstr(vlogs_msg, "prev_gt911=0x5D boots=15 rtc_sync=synced rtc_sync_age=0s") != nullptr);

  char vmetrics_prom[448];
  int plen = brd_power::format_vmetrics_prometheus(vmetrics_prom, sizeof(vmetrics_prom), telem);
  assert(plen > 0 && static_cast<size_t>(plen) < sizeof(vmetrics_prom));
  assert(vmetrics_prom[plen - 1] == '\n');
  assert(std::strstr(vmetrics_prom, "battery_voltage{job=\"brd\"} 4.12\n") != nullptr);
  assert(std::strstr(vmetrics_prom, "battery_percent{job=\"brd\"} 92\n") != nullptr);
  assert(std::strstr(vmetrics_prom, "gt911_slept{job=\"brd\"} 1\n") != nullptr);
  assert(std::strstr(vmetrics_prom, "boot_count{job=\"brd\"} 15\n") != nullptr);
  assert(std::strstr(vmetrics_prom, "rtc_synced{job=\"brd\"} 1\n") != nullptr);
  assert(std::strstr(vmetrics_prom, "rtc_sync_age_secs{job=\"brd\"} 0\n") != nullptr);

  // Telemetry formatting when RTC sync exceeded the timeout and next_in/since are unknown (no "unknowns").
  telem.next_in_str = "unknown";
  telem.since_str = "unknown";
  telem.rtc_sync = "timeout";
  telem.rtc_sync_code = -1;
  telem.rtc_sync_age_sec = -1;
  vlen = brd_power::format_vlogs_message(vlogs_msg, sizeof(vlogs_msg), telem);
  assert(vlen > 0 && static_cast<size_t>(vlen) < sizeof(vlogs_msg));
  assert(std::strstr(vlogs_msg, "next_in=unknown since=unknown") != nullptr);
  assert(std::strstr(vlogs_msg, "unknowns") == nullptr);
  assert(std::strstr(vlogs_msg, "rtc_sync=timeout rtc_sync_age=unknown") != nullptr);

  // Negative numeric next_in values receive the 's' suffix. Null fields return "unknown" without an extra 's'.
  brd_power::TraceTelemetry null_telem;
  null_telem.wake_reason = nullptr;
  null_telem.mode = nullptr;
  null_telem.image_status = nullptr;
  null_telem.now_str = nullptr;
  null_telem.next_str = nullptr;
  null_telem.next_in_str = nullptr;
  null_telem.since_str = nullptr;
  null_telem.job_name = nullptr;
  null_telem.rtc_sync = nullptr;
  vlen = brd_power::format_vlogs_message(vlogs_msg, sizeof(vlogs_msg), null_telem);
  assert(vlen > 0 && static_cast<size_t>(vlen) < sizeof(vlogs_msg));
  assert(std::strstr(vlogs_msg, "next_in=unknown since=unknown") != nullptr);
  assert(std::strstr(vlogs_msg, "unknowns") == nullptr);

  null_telem.next_in_str = "-15";
  null_telem.since_str = "0";
  vlen = brd_power::format_vlogs_message(vlogs_msg, sizeof(vlogs_msg), null_telem);
  assert(vlen > 0 && static_cast<size_t>(vlen) < sizeof(vlogs_msg));
  assert(std::strstr(vlogs_msg, "next_in=-15s since=0s") != nullptr);

  char vmetrics_prom_large[640];
  plen = brd_power::format_vmetrics_prometheus(vmetrics_prom_large, sizeof(vmetrics_prom_large), telem);
  assert(plen > 0 && static_cast<size_t>(plen) < sizeof(vmetrics_prom_large));
  assert(vmetrics_prom_large[plen - 1] == '\n');
  assert(std::strstr(vmetrics_prom_large, "rtc_synced{job=\"brd\"} -1\n") != nullptr);
  assert(std::strstr(vmetrics_prom_large, "rtc_sync_age_secs{job=\"brd\"} -1\n") != nullptr);

  // Prometheus label escaping and NaN battery handling.
  char escaped_label[32];
  brd_power::escape_prom_label("brd\"a\\b\nc\r", escaped_label, sizeof(escaped_label));
  assert(std::strcmp(escaped_label, "brd\\\"a\\\\b\\nc") == 0);

  brd_power::TraceTelemetry nan_telem = telem;
  nan_telem.job_name = "kitchen-epaper-display-west-01\"x";
  nan_telem.batt_v = std::numeric_limits<float>::quiet_NaN();
  nan_telem.batt_pct = std::numeric_limits<float>::quiet_NaN();
  nan_telem.awake_s = std::numeric_limits<float>::quiet_NaN();
  vlen = brd_power::format_vlogs_message(vlogs_msg, sizeof(vlogs_msg), nan_telem);
  assert(vlen > 0 && static_cast<size_t>(vlen) < sizeof(vlogs_msg));
  assert(std::strstr(vlogs_msg, "batt=unknown batt_pct=unknown awake=0.0s") != nullptr);
  plen = brd_power::format_vmetrics_prometheus(vmetrics_prom_large, sizeof(vmetrics_prom_large), nan_telem);
  assert(plen > 0 && static_cast<size_t>(plen) < sizeof(vmetrics_prom_large));
  assert(vmetrics_prom_large[plen - 1] == '\n');
  assert(std::strstr(vmetrics_prom_large, "battery_voltage") == nullptr);
  assert(std::strstr(vmetrics_prom_large, "battery_percent") == nullptr);
  assert(std::strstr(vmetrics_prom_large, "nan") == nullptr);
  assert(std::strstr(vmetrics_prom_large, "wake_secs{job=\"kitchen-epaper-display-west-01\\\"x\"} 0.0\n") != nullptr);

  // Prometheus batt_lines truncation resets NUL cleanly instead of emitting a partial metric.
  brd_power::TraceTelemetry long_job_telem = telem;
  const std::string long_job(95, 'j');
  long_job_telem.job_name = long_job.c_str();
  long_job_telem.batt_v = 123456.78f;
  long_job_telem.batt_pct = 1234567890.0f;
  char vmetrics_prom_huge[2048];
  plen = brd_power::format_vmetrics_prometheus(vmetrics_prom_huge, sizeof(vmetrics_prom_huge),
                                               long_job_telem);
  assert(plen > 0 && static_cast<size_t>(plen) < sizeof(vmetrics_prom_huge));
  assert(vmetrics_prom_huge[plen - 1] == '\n');
  assert(std::strstr(vmetrics_prom_huge, "battery_voltage{job=\"") != nullptr);
  assert(std::strstr(vmetrics_prom_huge, "battery_percent") == nullptr);
  const char *wake_pos = std::strstr(vmetrics_prom_huge, "wake_secs{job=\"");
  assert(wake_pos != nullptr);
  assert(wake_pos > vmetrics_prom_huge && wake_pos[-1] == '\n');

  printf("  -> Passed power state and telemetry checks.\n");
}

static void test_multithreaded_concurrency() {
  printf("[TEST] Multithreaded concurrency test (6 threads, 1000 iterations)...\n");

  brd_blit::BlitBuffer shared_buffer;
  constexpr int ITERATIONS = 1000;

  std::thread t1([&shared_buffer]() {
    std::vector<uint8_t> data(brd_blit::FRAMEBUFFER_SIZE);
    for (int i = 0; i < ITERATIONS; ++i) {
      std::memset(data.data(), static_cast<uint8_t>(i & 0xFF), data.size());
      assert(shared_buffer.store(data.data(), data.size()));
    }
  });

  std::thread t2([&shared_buffer]() {
    std::vector<uint8_t> data(brd_blit::FRAMEBUFFER_SIZE);
    for (int i = 0; i < ITERATIONS; ++i) {
      if (shared_buffer.retrieve(data.data(), data.size())) {
        assert(std::memcmp(data.data(), data.data() + 1, data.size() - 1) == 0);
      }
    }
  });

  std::thread t3([]() {
    brd_schedule::PosixTzCalendar cal("CET-1CEST,M3.5.0,M10.5.0/3");
    brd_schedule::ScheduleConfig cfg;
    for (int i = 0; i < ITERATIONS; ++i) {
      int64_t epoch = 1782000000 + i * 60;
      auto plan = brd_schedule::plan_next_wake(cal, epoch, cfg);
      assert(plan.clock_valid);
    }
  });

  std::thread t4([]() {
    brd_power::RtcPersistentState local_state;
    local_state.reset();
    for (int i = 0; i < ITERATIONS; ++i) {
      local_state.boot_count = static_cast<uint32_t>(i);
      local_state.update_crc();
      assert(local_state.validate_or_reset());
    }
  });

  std::thread t5([]() {
    brd_schedule::PosixTzCalendar cal_ny("EST5EDT,M3.2.0,M11.1.0");
    for (int i = 0; i < ITERATIONS; ++i) {
      int64_t epoch = 1782000000 + i * 60;
      brd_schedule::WallTime wt;
      assert(cal_ny.epoch_to_local(epoch, wt));
      int64_t back_epoch = 0;
      assert(cal_ny.local_to_epoch(wt, back_epoch));
      assert(back_epoch == epoch);
    }
  });

  std::thread t6([]() {
    brd_schedule::PosixTzCalendar cal_utc("UTC");
    for (int i = 0; i < ITERATIONS; ++i) {
      int64_t epoch = 1782000000 + i * 60;
      brd_schedule::WallTime wt;
      assert(cal_utc.epoch_to_local(epoch, wt));
      int64_t back_epoch = 0;
      assert(cal_utc.local_to_epoch(wt, back_epoch));
      assert(back_epoch == epoch);
    }
  });

  t1.join();
  t2.join();
  t3.join();
  t4.join();
  t5.join();
  t6.join();

  printf("  -> Passed multithreaded concurrency test cleanly.\n");
}

int main() {
  printf("=========================================\n");
  printf("Starting brd test suite (C++20)\n");
  printf("=========================================\n");

  test_blit_bounds_and_errors();
  test_blit_sha256_normalizer();
  test_blit_thread_safe_buffer();
  test_schedule_civil_normalization();
  test_schedule_dst_and_boundaries();
  test_schedule_dst_edge_cases();
  test_rtc_bcd_and_retries();
  test_rtc_arm_wake_protocol();
  test_power_state_and_telemetry();
  test_power_state_and_usb();
  test_power_telemetry();
  test_multithreaded_concurrency();

  printf("=========================================\n");
  printf("ALL BRD TESTS PASSED (0 ERRORS)\n");
  printf("=========================================\n");
  return 0;
}
