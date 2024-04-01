// curl -X GET http://localhost:3001/call


const express = require('express');
const nodemailer = require('nodemailer');

const app = express();
const PORT = process.env.PORT || 3051;


// Define the API endpoint for triggering the SMTP call
app.get('/call', async (req, res) => {

// Configure Nodemailer transporter
const transporter = nodemailer.createTransport({
  host: 'smtp.gmail.com',
  port: 465,
  secure: true,
  auth: {
    user: 'ahmadlotfygamersfield@gmail.com',
     pass: "", // Use the app password here
  },
});
  try {
    // Send email
    const info = await transporter.sendMail({
      from: '"You" <ahmadlotfygamersfield@gmail.com>',
      to: 'lotfyforloop@gmail.com',
      subject: 'Testing, testing, 123',
      html: `
        <h1>Hello there</h1>
        <p>Isn't NodeMailer useful?</p>
      `,
    });

    console.log('From App Email sent:', info.messageId);
    res.status(200).send('Email sent successfully');
  } catch (error) {
    console.error('Error sending email:', error);
    res.status(500).send('Error sending email');
  }
});

// Start the server
app.listen(PORT, () => {
  console.log(`Server is running on http://localhost:${PORT}`);
});
