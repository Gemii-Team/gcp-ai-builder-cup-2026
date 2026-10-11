import type { UiLabels } from './en';

// Thai. A Latin word inside a Thai string is marked `<en>word</en>` so it renders `lang="en"`; the Thai around it is
// tagged `lang="th"` by the renderer. REF codes and counted numbers are passed in as parameters and stay literal.
export const th: UiLabels = {
  // Proof slip
  generated: 'สร้างเมื่อ {time}',
  sources: 'แหล่งที่มา',
  draftsToConfirm: { one: '{count} ฉบับร่างรอยืนยัน', other: '{count} ฉบับร่างรอยืนยัน' },
  noDraftsWaiting: 'ไม่มีฉบับร่างที่รออยู่',
  decision: 'ข้อตัดสินใจ',
  openQuestion: 'คำถามที่ค้างอยู่',

  // Failed
  failed: 'ล้มเหลวเมื่อ {time}',
  retry: 'ลองอีกครั้ง',

  // Pending
  answerProgress: 'ความคืบหน้าของคำตอบ',
  retrieving: { one: 'กำลังค้นหาจาก {count} แชนเนล…', other: 'กำลังค้นหาจาก {count} แชนเนล…' },
  retrieved: { one: 'ค้นหาจาก {count} แชนเนลแล้ว', other: 'ค้นหาจาก {count} แชนเนลแล้ว' },
  writingAnswer: 'เขียนคำตอบ',
  writingAnswerNow: 'กำลังเขียนคำตอบ…',

  // Offset, page and message links; citation markers
  transcriptAt: 'บทถอดเสียงที่ {time}',
  pageShort: 'หน้า {page}',
  pageOf: '{file} หน้า {page}',
  messageIn: 'ข้อความใน #{channel} เวลา {time}',
  messageInUntimed: 'ข้อความใน #{channel}',
  sourceN: 'แหล่งที่มา {n}',
  sourceNOf: 'แหล่งที่มา {n}: {label}',

  // Transcript excerpt
  close: 'ปิด',
  excerptLoading: 'กำลังโหลดข้อความตัดตอน…',
  excerptError: 'โหลดข้อความตัดตอนนี้ไม่ได้',
  transcriptExcerptAt: 'ข้อความตัดตอนจากบทถอดเสียงที่ {time}',
  transcriptExcerpt: 'ข้อความตัดตอนจากบทถอดเสียง',
  documentExcerpt: 'ข้อความตัดตอนจากเอกสาร {file} หน้า {page}',
  documentExcerptNoPage: 'ข้อความตัดตอนจากเอกสาร {file}',
  messageExcerpt: 'ข้อความตัดตอนจากข้อความของ {author}',
  transcriptHeader: 'บทถอดเสียง · {title} · {time}',
  transcriptHeaderUntimed: 'บทถอดเสียง · {title}',
  messageHeader: 'ข้อความ · #{channel} · {day} {time}',
  messageHeaderToday: 'ข้อความ · #{channel} · {time}',
  documentHeader: 'เอกสาร · {file} · หน้า {page}',
  documentPage: 'หน้า {page} จาก {pages} · {ref}',
  documentPageNoCount: 'หน้า {page} · {ref}',

  // Seal
  confirmed: 'ยืนยันแล้ว',
  confirmedStamp: 'ยืนยันแล้ว',

  // Draft slip and owner select
  draft: 'ฉบับร่าง',
  confirmAction: 'ยืนยัน',
  confirmNamed: 'ยืนยัน: {title}',
  edit: 'แก้ไข',
  editNamed: 'แก้ไข: {title}',
  save: 'บันทึก',
  cancel: 'ยกเลิก',
  taskTitle: 'ชื่องาน',
  titleEmpty: 'ชื่องานต้องไม่ว่าง',
  owner: 'ผู้รับผิดชอบ',
  chooseOwner: 'เลือกผู้รับผิดชอบ',
  chooseOwnerFirst: 'เลือกผู้รับผิดชอบก่อน',
  noOwner: 'ไม่มีผู้รับผิดชอบ',
  noDueDate: 'ไม่มีกำหนดส่ง',
  fromMeeting: 'จากการประชุม',
  fromMessage: 'จากข้อความ',
  fromAnswer: 'จากคำตอบ {ref}',
  handoffNote: 'ยังเป็นฉบับร่างจนกว่าจะมีคนยืนยันบนบอร์ด',
  reviewOnBoard: 'ตรวจดูบนบอร์ด',
  discard: 'ทิ้งฉบับร่าง',
  confirmFailed: 'ยืนยันฉบับร่างนี้ไม่ได้ ลองอีกครั้ง',
  saveFailed: 'บันทึกการแก้ไขไม่ได้ ลองอีกครั้ง',

  // Sealed slip
  confirmedBy: 'ยืนยันโดย',

  // Drafts panel, band and bar
  aiDrafts: 'ฉบับร่างจาก <en>AI</en>',
  draftsCaption: 'ไม่มีอะไรมีผลจนกว่าคุณจะยืนยัน',
  toConfirm: '{count} รอยืนยัน',
  allConfirmed: 'ยืนยันครบแล้ว',
  draftsList: 'ฉบับร่างที่รอยืนยัน',
  confirmedJustNow: 'เพิ่งยืนยัน',
  draftsEmptyBody: 'ยืนยันฉบับร่างจาก <en>AI</en> ครบแล้ว',
  board: 'บอร์ด',
  todo: 'ต้องทำ',
  doing: 'กำลังทำ',
  done: 'เสร็จแล้ว',
  confirmedAnnouncement: {
    one: 'ยืนยันแล้ว: {title} เหลือฉบับร่าง {count} ฉบับ',
    other: 'ยืนยันแล้ว: {title} เหลือฉบับร่าง {count} ฉบับ',
  },
  confirmedAnnouncementLast: 'ยืนยันแล้ว: {title} ยืนยันฉบับร่างครบทุกฉบับแล้ว',

  // Task form and created card
  taskFieldTitle: 'ชื่องาน',
  taskFieldChannel: 'แชนเนล',
  dueDate: 'กำหนดส่ง',
  createTask: 'สร้างงาน',
  createFailed: 'สร้างงานไม่ได้ ลองอีกครั้ง',
  taskNamed: 'งาน {code}',
  taskCreated: 'สร้างงาน {code} แล้ว · {link}',
  viewOnBoard: 'ดูบนบอร์ด',
};
