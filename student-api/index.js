import express from 'express';
import fs from 'fs';
import bodyParser from 'body-parser';

const app = express();
const PORT = 3000;

app.use(bodyParser.json());

// Utility functions
const readDB = () => JSON.parse(fs.readFileSync('db.json'));
const writeDB = (data) => fs.writeFileSync('db.json', JSON.stringify(data, null, 2));

// GET all students
app.get('/students', (req, res) => {
  const db = readDB();
  res.json(db.students);
});

// GET student by ID
app.get('/students/:id', (req, res) => {
  const db = readDB();
  const student = db.students.find(s => s.id == req.params.id);
  student ? res.json(student) : res.status(404).json({ message: 'Student not found' });
});

// POST a new student
app.post('/students', (req, res) => {
  const db = readDB();
  const newStudent = {
    id: Date.now(),
    name: req.body.name,
    email: req.body.email,
    course: req.body.course
  };
  db.students.push(newStudent);
  writeDB(db);
  res.status(201).json(newStudent);
});

// PUT (update) a student
app.put('/students/:id', (req, res) => {
  const db = readDB();
  const index = db.students.findIndex(s => s.id == req.params.id);
  if (index !== -1) {
    db.students[index] = { ...db.students[index], ...req.body };
    writeDB(db);
    res.json(db.students[index]);
  } else {
    res.status(404).json({ message: 'Student not found' });
  }
});

// DELETE a student
app.delete('/students/:id', (req, res) => {
  const db = readDB();
  const newStudents = db.students.filter(s => s.id != req.params.id);
  if (newStudents.length !== db.students.length) {
    db.students = newStudents;
    writeDB(db);
    res.json({ message: 'Student deleted' });
  } else {
    res.status(404).json({ message: 'Student not found' });
  }
});

app.listen(PORT, () => {
  console.log(`✅ Server running at http://localhost:${PORT}`);
});
