import type { I18nTable } from './en';

// Thai. A Latin word inside a Thai string is marked `<en>word</en>` so `Rich` renders it `lang="en"`; the Thai around
// it is tagged `lang="th"` by the renderer. Parameters ({workspace}, {domain}, counts) are filled in by the caller.
export const th: I18nTable = {
  // Shared
  draftsToConfirm: { one: '{count} ฉบับร่างรอยืนยัน', other: '{count} ฉบับร่างรอยืนยัน' },

  // Sign in
  signInDocumentTitle: 'ลงชื่อเข้าใช้ · <en>K.I.W.I.</en>',
  signInTagline: 'ช่องสนทนา การประชุม และไฟล์ของทีมคุณ พร้อม <en>AI</en> ที่แสดงแหล่งที่มา',
  signInTitle: 'ลงชื่อเข้าใช้ {workspace}',
  signInTitleDefault: 'ลงชื่อเข้าใช้ <en>K.I.W.I.</en>',
  signInContinueWithGoogle: 'ดำเนินการต่อด้วย <en>Google</en>',
  signInRedirecting: 'กำลังเปลี่ยนเส้นทางไปยัง <en>Google</en>…',
  signInHint: 'ใช้บัญชี <en>Google</en> {domain} ของคุณ',
  signInSeeHeading: 'สิ่งที่ <en>K.I.W.I.</en> จะเห็น',
  signInSeeChannels: 'ช่องที่คุณเข้าร่วมและข้อความในช่องนั้น',
  signInSeeFiles: 'ไฟล์ที่คุณวางลงในช่อง',
  signInSeeMeet: 'บทถอดเสียงจาก <en>Meet</en> ของการประชุมที่คุณเริ่มใน <en>K.I.W.I.</en>',
  signInSeeAsks: '<en>K.I.W.I.</en> จะถามก่อนอ่าน <en>Drive</en> หรือร่างอีเมลใน <en>Gmail</en>',
  signInNotInWorkspace: 'ไม่ได้อยู่ใน <en>Google Workspace</en> ของ {workspace} ลงชื่อเข้าใช้ด้วยบัญชีที่ทำงานของคุณ',
  signInSignedOut: 'คุณออกจากระบบแล้ว',
};
